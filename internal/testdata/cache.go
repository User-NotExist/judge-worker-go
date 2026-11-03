// Package testdata downloads, verifies, unpacks and caches problem packages (§9).
package testdata

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PackageError is a permanent package problem: the job ends SE with Reason (§14).
type PackageError struct {
	Reason string // package_hash_mismatch | invalid_package | checker_compile_failed | manager_compile_failed
	Detail string
}

func (e *PackageError) Error() string { return e.Reason + ": " + e.Detail }

// CompileError means the checker/manager source did not compile (as opposed to a sandbox failure).
type CompileError struct{ Log string }

func (e *CompileError) Error() string { return "compile failed" }

// Fetcher downloads an object from the problems bucket to a local file.
type Fetcher interface {
	Fetch(ctx context.Context, key, dst string) error
}

// Compiler compiles a trusted helper (checker or manager) in go-judge and returns the binary.
type Compiler func(ctx context.Context, source []byte) ([]byte, error)

type Package struct {
	SHA256  string
	Dir     string
	Problem *Problem
	release func()
}

func (p *Package) CheckerPath() string { return filepath.Join(p.Dir, "bin", "checker") }
func (p *Package) ManagerPath() string { return filepath.Join(p.Dir, "bin", "manager") }
func (p *Package) InputPath(tc string) string {
	return filepath.Join(p.Dir, "tests", tc+".in")
}
func (p *Package) AnswerPath(tc string) string {
	return filepath.Join(p.Dir, "tests", tc+".ans")
}

// Release drops the shared lock that protects the package from eviction.
func (p *Package) Release() {
	if p.release != nil {
		p.release()
		p.release = nil
	}
}

type Cache struct {
	Dir      string
	MaxBytes uint64
	Fetch    Fetcher
	Compile  Compiler
	Log      *slog.Logger
	// OnSize is called with the total cache size after every scan (metrics gauge).
	OnSize func(bytes uint64)

	evictMu sync.Mutex
}

func (c *Cache) lockPath(sha string) string { return filepath.Join(c.Dir, ".lock", sha) }
func (c *Cache) pkgDir(sha string) string   { return filepath.Join(c.Dir, sha) }
func (c *Cache) invalidPath(sha string) string {
	return filepath.Join(c.Dir, ".invalid", sha)
}

// Init creates the cache skeleton and removes leftovers from a crash.
func (c *Cache) Init() error {
	for _, d := range []string{c.Dir, filepath.Join(c.Dir, ".lock"), filepath.Join(c.Dir, ".tmp"), filepath.Join(c.Dir, ".invalid")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	tmp := filepath.Join(c.Dir, ".tmp")
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
	}
	go c.Evict()
	return nil
}

func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			return err
		}
	}
}

func (c *Cache) ready(sha string) bool {
	_, err := os.Stat(filepath.Join(c.pkgDir(sha), ".ready"))
	return err == nil
}

// Ensure makes the package available and returns it holding a shared lock; call Release.
func (c *Cache) Ensure(ctx context.Context, sha, key string) (*Package, error) {
	if b, err := os.ReadFile(c.invalidPath(sha)); err == nil {
		r, d, _ := strings.Cut(string(b), "\n")
		return nil, &PackageError{Reason: r, Detail: d}
	}
	lf, err := openLock(c.lockPath(sha))
	if err != nil {
		return nil, err
	}
	release := func() { _ = flock(lf, syscall.LOCK_UN); _ = lf.Close() }
	if err := flock(lf, syscall.LOCK_SH); err != nil {
		release()
		return nil, err
	}
	if !c.ready(sha) {
		// Upgrade to exclusive and re-check (another goroutine may have finished meanwhile).
		if err := flock(lf, syscall.LOCK_EX); err != nil {
			release()
			return nil, err
		}
		if !c.ready(sha) {
			if err := c.build(ctx, sha, key); err != nil {
				release()
				var pe *PackageError
				if errors.As(err, &pe) && pe.Reason != "package_hash_mismatch" {
					c.markInvalid(sha, pe)
				}
				return nil, err
			}
			go c.Evict()
		}
		if err := flock(lf, syscall.LOCK_SH); err != nil {
			release()
			return nil, err
		}
	}
	dir := c.pkgDir(sha)
	now := time.Now()
	_ = os.Chtimes(filepath.Join(dir, ".ready"), now, now) // LRU touch
	p, err := ParseProblem(dir)
	if err != nil {
		release()
		return nil, &PackageError{Reason: "invalid_package", Detail: err.Error()}
	}
	return &Package{SHA256: sha, Dir: dir, Problem: p, release: release}, nil
}

func (c *Cache) markInvalid(sha string, pe *PackageError) {
	_ = os.WriteFile(c.invalidPath(sha), []byte(pe.Reason+"\n"+pe.Detail), 0o640)
}

func randName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// build runs steps 3–6 of Ensure under the exclusive lock.
func (c *Cache) build(ctx context.Context, sha, key string) error {
	start := time.Now()
	tmpRoot := filepath.Join(c.Dir, ".tmp")
	name := randName()
	zipPath := filepath.Join(tmpRoot, name+".zip")
	work := filepath.Join(tmpRoot, name)
	defer os.Remove(zipPath)
	defer os.RemoveAll(work)

	if err := c.Fetch.Fetch(ctx, key, zipPath); err != nil {
		return fmt.Errorf("download package: %w", err)
	}
	got, err := fileSHA256(zipPath)
	if err != nil {
		return err
	}
	if got != sha {
		return &PackageError{Reason: "package_hash_mismatch", Detail: "got " + got}
	}
	if err := os.MkdirAll(work, 0o750); err != nil {
		return err
	}
	if err := Unzip(zipPath, work); err != nil {
		if isDiskFull(err) {
			return err
		}
		return &PackageError{Reason: "invalid_package", Detail: err.Error()}
	}
	p, err := ParseProblem(work)
	if err != nil {
		return &PackageError{Reason: "invalid_package", Detail: err.Error()}
	}
	if err := os.MkdirAll(filepath.Join(work, "bin"), 0o750); err != nil {
		return err
	}
	helpers := []struct{ src, bin, reason string }{}
	if p.HasChecker() {
		helpers = append(helpers, struct{ src, bin, reason string }{"checker/checker.cpp", "bin/checker", "checker_compile_failed"})
	}
	if p.HasManager() {
		helpers = append(helpers, struct{ src, bin, reason string }{"manager/manager.cpp", "bin/manager", "manager_compile_failed"})
	}
	for _, h := range helpers {
		src, err := os.ReadFile(filepath.Join(work, h.src))
		if err != nil {
			return &PackageError{Reason: "invalid_package", Detail: err.Error()}
		}
		bin, err := c.Compile(ctx, src)
		if err != nil {
			var ce *CompileError
			if errors.As(err, &ce) {
				return &PackageError{Reason: h.reason, Detail: firstLines(ce.Log, 20)}
			}
			return err // sandbox problem: transient, do not mark invalid
		}
		if err := writeFileSync(filepath.Join(work, h.bin), bin, 0o750); err != nil {
			return err
		}
	}
	if err := writeFileSync(filepath.Join(work, ".ready"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o640); err != nil {
		return err
	}
	if err := syncDir(work); err != nil {
		return err
	}
	dst := c.pkgDir(sha)
	_ = os.RemoveAll(dst) // leftover without .ready
	if err := os.Rename(work, dst); err != nil {
		return err
	}
	_ = syncDir(c.Dir)
	c.Log.Info("package cached", "package_sha256", sha, "testcases", len(p.Testcases), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode) // umask-independent
}

func syncDir(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}

func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// IsDiskFull reports whether err is ENOSPC (§14 "Disk full").
func IsDiskFull(err error) bool { return isDiskFull(err) }

func dirSize(dir string) uint64 {
	var n uint64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += uint64(fi.Size())
			}
		}
		return nil
	})
	return n
}

// Evict deletes least-recently-used packages until the cache is under MaxBytes,
// skipping any package locked by a running job (§9 step 7). target < 1 uses MaxBytes.
func (c *Cache) Evict() { c.EvictTo(c.MaxBytes) }

func (c *Cache) EvictTo(target uint64) {
	c.evictMu.Lock()
	defer c.evictMu.Unlock()
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return
	}
	type pkg struct {
		sha   string
		size  uint64
		mtime time.Time
	}
	var pkgs []pkg
	var total uint64
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(c.Dir, e.Name())
		st, err := os.Stat(filepath.Join(dir, ".ready"))
		if err != nil {
			continue // in progress or broken; build() replaces it
		}
		sz := dirSize(dir)
		total += sz
		pkgs = append(pkgs, pkg{e.Name(), sz, st.ModTime()})
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].mtime.Before(pkgs[j].mtime) })
	for _, p := range pkgs {
		if total <= target {
			break
		}
		lf, err := openLock(c.lockPath(p.sha))
		if err != nil {
			continue
		}
		if flock(lf, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			lf.Close() // in use by a running job
			continue
		}
		dir := c.pkgDir(p.sha)
		_ = os.Remove(filepath.Join(dir, ".ready"))
		if err := os.RemoveAll(dir); err == nil {
			total -= p.size
			c.Log.Info("package evicted", "package_sha256", p.sha, "bytes", p.size)
		}
		_ = flock(lf, syscall.LOCK_UN)
		lf.Close()
	}
	if c.OnSize != nil {
		c.OnSize(total)
	}
}
