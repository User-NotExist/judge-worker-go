// Package pipeline orchestrates compile → run → check → score for every task type (§10).
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"judge-worker/internal/config"
	"judge-worker/internal/gojudge"
	"judge-worker/internal/job"
	"judge-worker/internal/lang"
	"judge-worker/internal/metrics"
	"judge-worker/internal/pkgcache"
	"judge-worker/internal/report"
)

// Deps are shared by all jobs on this worker.
type Deps struct {
	Cfg     *config.Config
	GJ      gojudge.Client
	Langs   *lang.Registry
	Cache   *pkgcache.Cache
	HTTP    *http.Client // presigned URLs
	Metrics *metrics.M

	mu             sync.RWMutex
	goJudgeVersion string
	toolchains     map[string]string
}

func (d *Deps) SetGoJudgeVersion(v string) {
	d.mu.Lock()
	d.goJudgeVersion = v
	d.mu.Unlock()
}

func (d *Deps) GoJudgeVersion() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.goJudgeVersion
}

func (d *Deps) SetToolchain(langID, v string) {
	d.mu.Lock()
	if d.toolchains == nil {
		d.toolchains = map[string]string{}
	}
	d.toolchains[langID] = v
	d.mu.Unlock()
}

func (d *Deps) Toolchain(langID string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.toolchains[langID]
}

// Failure classes. Each maps to a different end-of-job behaviour (§14).

// sysError → done{SE, reason}.
type sysError struct {
	reason string
	detail string
}

func (e *sysError) Error() string { return e.reason + ": " + e.detail }

func sysErr(reason string, f string, a ...any) error {
	return &sysError{reason: reason, detail: fmt.Sprintf(f, a...)}
}

// judgeError → done{JE, score null}.
type judgeError struct{ detail string }

func (e *judgeError) Error() string { return "judge error: " + e.detail }

// errTransient: infrastructure the job depends on is unavailable (go-judge, storage).
// No done, no ack: the message is reclaimed later as a new attempt.
type transientError struct {
	reason string
	err    error
}

func (e *transientError) Error() string { return e.reason + ": " + e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

var errSuperseded = errors.New("attempt superseded (409)")

// jobCtx is the per-job state shared by every step.
type jobCtx struct {
	d       *Deps
	job     *job.Job
	log     *slog.Logger
	rep     *report.Reporter
	scratch string
	pkg     *pkgcache.Package
	lang    *lang.Language

	fileIDs map[string]bool // go-judge file-store ids to delete at job end
	sources map[string][]byte
	binary  string // fileId of compiled artifact
}

func (d *Deps) newJobCtx(j *job.Job, log *slog.Logger, rep *report.Reporter) (*jobCtx, error) {
	scratch := filepath.Join(d.Cfg.ScratchDir, j.JobID)
	_ = os.RemoveAll(scratch)
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		return nil, err
	}
	return &jobCtx{d: d, job: j, log: log, rep: rep, scratch: scratch, fileIDs: map[string]bool{}}, nil
}

func (jc *jobCtx) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for id := range jc.fileIDs {
		if err := jc.d.GJ.DeleteFile(ctx, id); err != nil {
			jc.log.Debug("delete go-judge file", "file_id", id, "err", err)
		}
	}
	if jc.pkg != nil {
		jc.pkg.Release()
	}
	_ = os.RemoveAll(jc.scratch)
}

func (jc *jobCtx) emit(ctx context.Context, typ string, payload any) error {
	if jc.rep == nil {
		return nil
	}
	if jc.rep.Emit(ctx, typ, payload) == report.Superseded {
		return errSuperseded
	}
	return nil
}

func (jc *jobCtx) dropFile(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if err := jc.d.GJ.DeleteFile(ctx, id); err == nil {
		delete(jc.fileIDs, id)
	}
}

// run calls go-judge, records cached file ids and classifies transport failures.
func (jc *jobCtx) run(ctx context.Context, req gojudge.Request) ([]gojudge.Result, error) {
	res, err := jc.d.GJ.Run(ctx, req)
	if err != nil {
		if errors.Is(err, gojudge.ErrUnavailable) {
			return nil, &transientError{reason: "sandbox_unavailable", err: err}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, sysErr("sandbox_error", "%v", err)
	}
	for _, r := range res {
		for _, id := range r.FileIDs {
			jc.fileIDs[id] = true
		}
		switch r.Status {
		case gojudge.StatusInternalError:
			jc.d.Metrics.GoJudgeErrors.WithLabelValues("internal").Inc()
		case gojudge.StatusFileError:
			jc.d.Metrics.GoJudgeErrors.WithLabelValues("file").Inc()
		}
	}
	return res, nil
}

// runRetry runs req and retries once if any command ended in File Error / Internal Error.
func (jc *jobCtx) runRetry(ctx context.Context, req gojudge.Request) ([]gojudge.Result, error) {
	var res []gojudge.Result
	var err error
	for try := 0; try < 2; try++ {
		res, err = jc.run(ctx, req)
		if err != nil {
			return nil, err
		}
		retry := false
		for _, r := range res {
			if r.Status == gojudge.StatusInternalError || r.Status == gojudge.StatusFileError {
				retry = true
				jc.log.Warn("go-judge step failed; retrying", "status", r.Status, "error", r.Error, "try", try+1)
			}
		}
		if !retry {
			return res, nil
		}
		for _, r := range res {
			for _, id := range r.FileIDs {
				jc.dropFile(ctx, id)
			}
		}
	}
	for _, r := range res {
		if r.Status == gojudge.StatusInternalError || r.Status == gojudge.StatusFileError {
			return nil, sysErr("sandbox_"+map[string]string{gojudge.StatusInternalError: "internal_error", gojudge.StatusFileError: "file_error"}[r.Status], "%s", r.Error)
		}
	}
	return res, nil
}

// downloadFiles fetches presigned source files and verifies SHA-256.
func (jc *jobCtx) downloadFiles(ctx context.Context, files []job.FileRef, maxBytes int64) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		var b []byte
		var err error
		for try := 0; try < 3; try++ {
			if try > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(try) * time.Second):
				}
			}
			b, err = jc.fetch(ctx, f.URL, maxBytes)
			if err == nil || errors.Is(err, errPermanent) {
				break
			}
		}
		if err != nil {
			if errors.Is(err, errPermanent) {
				return nil, sysErr("source_download_failed", "%s: %v", f.Name, err)
			}
			return nil, &transientError{reason: "storage_unavailable", err: err}
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, sysErr("source_hash_mismatch", "%s", f.Name)
		}
		out[f.Name] = b
	}
	return out, nil
}

var errPermanent = errors.New("permanent")

func (jc *jobCtx) fetch(ctx context.Context, url string, max int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPermanent, err)
	}
	resp, err := jc.d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return nil, fmt.Errorf("%w: %v", errPermanent, err) // e.g. 403 expired URL
		}
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w: file larger than %d bytes", errPermanent, max)
	}
	return b, nil
}

// ensurePackage loads the problem package into the cache and validates it for this job.
func (jc *jobCtx) ensurePackage(ctx context.Context, o pkgcache.ValidateOpts) error {
	p, err := jc.d.Cache.Ensure(ctx, jc.job.Problem.PackageSHA256, jc.job.Problem.PackageKey)
	if err != nil {
		var pe *pkgcache.PackageError
		switch {
		case errors.As(err, &pe):
			jc.d.Metrics.Alert(pe.Reason)
			return sysErr(pe.Reason, "%s", pe.Detail)
		case errors.Is(err, gojudge.ErrUnavailable):
			return &transientError{reason: "sandbox_unavailable", err: err}
		case pkgcache.IsDiskFull(err):
			return &transientError{reason: "disk_full", err: err}
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			return &transientError{reason: "package_unavailable", err: err}
		}
	}
	jc.pkg = p
	if err := p.Problem.ValidateFor(p.Dir, o); err != nil {
		jc.d.Metrics.Alert("invalid_package")
		return sysErr("invalid_package", "%v", err)
	}
	return nil
}
