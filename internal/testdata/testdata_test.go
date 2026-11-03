package testdata

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const goodYAML = `type: batch
limits:
  memory_mb: 256
  time_ms: { cpp20-gcc13: 1000, py312: 3000 }
checker: { kind: default }
testcases:
  - { name: "1", sample: true }
  - { name: "2" }
  - { name: "3" }
subtasks:
  - { index: 0, points: 0, mode: min, testcases: ["1"] }
  - { index: 1, points: 30, mode: min, testcases: ["2"] }
  - { index: 2, points: 70, testcases: ["2", "3"], depends_on: [1] }
`

func writePkg(t *testing.T, yaml string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "problem.yaml"), []byte(yaml), 0o640); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		p := filepath.Join(dir, n)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, []byte(c), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func allTests() map[string]string {
	return map[string]string{"tests/1.in": "", "tests/1.ans": "", "tests/2.in": "", "tests/2.ans": "", "tests/3.in": "", "tests/3.ans": ""}
}

func TestProblemValid(t *testing.T) {
	dir := writePkg(t, goodYAML, allTests())
	p, err := ParseProblem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subtasks[2].Mode != "min" {
		t.Fatal("default mode")
	}
	if err := p.ValidateFor(dir, ValidateOpts{Language: "py312", RequireAnswers: true}); err != nil {
		t.Fatal(err)
	}
	if got := p.SubtasksOf("2"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatal(got)
	}
	if !p.IsSample("1") || p.IsSample("2") {
		t.Fatal("sample")
	}
}

func TestProblemRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		files  func(map[string]string)
		lang   string
		want   string
	}{
		{"missing language limit", nil, nil, "java", "no entry for java"},
		{"unknown testcase in subtask", func(s string) string {
			return strings.Replace(s, `testcases: ["1"]`, `testcases: ["9"]`, 1)
		}, nil, "", `testcase "9" does not exist`},
		{"missing .in", nil, func(m map[string]string) { delete(m, "tests/2.in") }, "", "tests/2.in missing"},
		{"missing .ans", nil, func(m map[string]string) { delete(m, "tests/3.ans") }, "py312", "tests/3.ans missing"},
		{"negative points", func(s string) string { return strings.Replace(s, "points: 30", "points: -1", 1) }, nil, "", "non-negative integer"},
		{"non-integer points", func(s string) string { return strings.Replace(s, "points: 30", "points: 2.5", 1) }, nil, "", "problem.yaml"},
		{"depends on later", func(s string) string {
			return strings.Replace(s, "depends_on: [1]", "depends_on: [2]", 1)
		}, nil, "", "earlier subtask"},
		{"testlib without checker", func(s string) string { return strings.Replace(s, "kind: default", "kind: testlib", 1) }, nil, "", "checker/checker.cpp missing"},
		{"interactive without manager", func(s string) string { return strings.Replace(s, "type: batch", "type: interactive", 1) }, nil, "", "manager/manager.cpp missing"},
		{"bad type", func(s string) string { return strings.Replace(s, "type: batch", "type: weird", 1) }, nil, "", "type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := goodYAML
			if c.mutate != nil {
				y = c.mutate(y)
			}
			files := allTests()
			if c.files != nil {
				c.files(files)
			}
			dir := writePkg(t, y, files)
			p, err := ParseProblem(dir)
			if err == nil {
				err = p.ValidateFor(dir, ValidateOpts{Language: c.lang, RequireAnswers: true})
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestInteractiveNeedsNoAns(t *testing.T) {
	y := strings.Replace(goodYAML, "type: batch", "type: interactive", 1)
	files := map[string]string{"tests/1.in": "", "tests/2.in": "", "tests/3.in": "", "manager/manager.cpp": "int main(){}"}
	dir := writePkg(t, y, files)
	p, err := ParseProblem(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateFor(dir, ValidateOpts{Language: "py312", RequireAnswers: true}); err != nil {
		t.Fatal(err)
	}
}

func makeZip(t *testing.T, entries map[string]string, extra func(*zip.Writer)) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range entries {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte(c))
	}
	if extra != nil {
		extra(zw)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestUnzipRejects(t *testing.T) {
	cases := map[string]func(*zip.Writer){
		"dotdot":    func(zw *zip.Writer) { w, _ := zw.Create("../evil"); w.Write([]byte("x")) },
		"nested ..": func(zw *zip.Writer) { w, _ := zw.Create("tests/../../evil"); w.Write([]byte("x")) },
		"absolute":  func(zw *zip.Writer) { w, _ := zw.Create("/etc/evil"); w.Write([]byte("x")) },
		"symlink": func(zw *zip.Writer) {
			h := &zip.FileHeader{Name: "link"}
			h.SetMode(os.ModeSymlink | 0o777)
			w, _ := zw.CreateHeader(h)
			w.Write([]byte("/etc/passwd"))
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "p.zip")
			_ = os.WriteFile(src, makeZip(t, nil, extra), 0o640)
			if err := Unzip(src, t.TempDir()); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	src := filepath.Join(t.TempDir(), "ok.zip")
	_ = os.WriteFile(src, makeZip(t, map[string]string{"problem.yaml": "x", "tests/1.in": "1"}, nil), 0o640)
	dst := t.TempDir()
	if err := Unzip(src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "tests/1.in"))
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatal(err, st.Mode())
	}
}

// fakeFetcher serves zips from memory and counts downloads.
type fakeFetcher struct {
	blobs map[string][]byte
	n     atomic.Int32
	delay time.Duration
}

func (f *fakeFetcher) Fetch(_ context.Context, key, dst string) error {
	f.n.Add(1)
	time.Sleep(f.delay)
	return os.WriteFile(dst, f.blobs[key], 0o640)
}

func pkgZip(t *testing.T) ([]byte, string) {
	ents := map[string]string{"problem.yaml": goodYAML}
	for k, v := range allTests() {
		ents[k] = v + strings.Repeat("x", 1000)
	}
	b := makeZip(t, ents, nil)
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

func newCache(t *testing.T, f *fakeFetcher, max uint64) *Cache {
	c := &Cache{Dir: t.TempDir(), MaxBytes: max, Fetch: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Compile: func(context.Context, []byte) ([]byte, error) { return []byte("bin"), nil }}
	if err := c.Init(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureConcurrentSingleDownload(t *testing.T) {
	b, sha := pkgZip(t)
	f := &fakeFetcher{blobs: map[string][]byte{"k": b}, delay: 50 * time.Millisecond}
	c := newCache(t, f, 1<<30)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := c.Ensure(context.Background(), sha, "k")
			if err != nil {
				errs <- err
				return
			}
			if len(p.Problem.Testcases) != 3 {
				errs <- os.ErrInvalid
			}
			p.Release()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := f.n.Load(); n != 1 {
		t.Fatalf("downloaded %d times", n)
	}
}

func TestEnsureHashMismatch(t *testing.T) {
	b, _ := pkgZip(t)
	c := newCache(t, &fakeFetcher{blobs: map[string][]byte{"k": b}}, 1<<30)
	_, err := c.Ensure(context.Background(), strings.Repeat("0", 64), "k")
	pe, ok := err.(*PackageError)
	if !ok || pe.Reason != "package_hash_mismatch" {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, strings.Repeat("0", 64))); !os.IsNotExist(err) {
		t.Fatal("cache entry must not be created")
	}
}

func TestCheckerCompileFailureMarksInvalid(t *testing.T) {
	y := strings.Replace(goodYAML, "kind: default", "kind: testlib", 1)
	ents := map[string]string{"problem.yaml": y, "checker/checker.cpp": "nope"}
	for k, v := range allTests() {
		ents[k] = v
	}
	b := makeZip(t, ents, nil)
	s := sha256.Sum256(b)
	sha := hex.EncodeToString(s[:])
	f := &fakeFetcher{blobs: map[string][]byte{"k": b}}
	c := newCache(t, f, 1<<30)
	c.Compile = func(context.Context, []byte) ([]byte, error) { return nil, &CompileError{Log: "error: nope"} }
	for i := 0; i < 2; i++ {
		_, err := c.Ensure(context.Background(), sha, "k")
		pe, ok := err.(*PackageError)
		if !ok || pe.Reason != "checker_compile_failed" {
			t.Fatal(err)
		}
	}
	if f.n.Load() != 1 {
		t.Fatal("invalid package should fail fast without re-downloading")
	}
}

func TestEvictionLRUSkipsLocked(t *testing.T) {
	f := &fakeFetcher{blobs: map[string][]byte{}}
	var shas []string
	for i := 0; i < 3; i++ {
		y := goodYAML + "\n# variant " + string(rune('a'+i)) + "\n"
		ents := map[string]string{"problem.yaml": y}
		for k, v := range allTests() {
			ents[k] = v + strings.Repeat("x", 10000)
		}
		b := makeZip(t, ents, nil)
		s := sha256.Sum256(b)
		sha := hex.EncodeToString(s[:])
		f.blobs[sha] = b
		shas = append(shas, sha)
	}
	c := newCache(t, f, 1<<30)
	var held *Package
	for i, sha := range shas {
		p, err := c.Ensure(context.Background(), sha, sha)
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(filepath.Join(p.Dir, ".ready"), old, old)
		if i == 0 {
			held = p // oldest, but in use
		} else {
			p.Release()
		}
	}
	c.EvictTo(1) // evict everything possible
	if _, err := os.Stat(held.Dir); err != nil {
		t.Fatal("locked package was evicted")
	}
	for _, sha := range shas[1:] {
		if _, err := os.Stat(filepath.Join(c.Dir, sha)); !os.IsNotExist(err) {
			t.Fatal("unlocked package not evicted")
		}
	}
	held.Release()
	c.EvictTo(1)
	if _, err := os.Stat(held.Dir); !os.IsNotExist(err) {
		t.Fatal("released package should now be evictable")
	}
}
