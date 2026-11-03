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
	"strings"
	"time"

	"judge-worker/internal/job"
	"judge-worker/internal/report"
	"judge-worker/internal/testdata"
	"judge-worker/internal/verdict"
)

// RunBuild generates .ans files from a reference solution (§10.7).
func (d *Deps) RunBuild(ctx context.Context, j *job.Job, slot int, rep *report.Reporter, log *slog.Logger) Outcome {
	start := time.Now()
	jc, err := d.newJobCtx(j, log, rep)
	if err != nil {
		return Outcome{Transient: true, TransientReason: "disk_full"}
	}
	defer jc.close()

	bd, err := jc.build(ctx)
	var te *transientError
	switch {
	case errors.Is(err, errSuperseded):
		return Outcome{Superseded: true}
	case errors.As(err, &te):
		log.Warn("build interrupted by infrastructure failure", "reason", te.reason, "err", te.err)
		return Outcome{Transient: true, TransientReason: te.reason}
	case err != nil && ctx.Err() != nil:
		return Outcome{Transient: true, TransientReason: "cancelled"}
	case err != nil:
		var se *sysError
		var je *judgeError
		switch {
		case errors.As(err, &se):
			bd = report.BuildDone{Reason: se.reason, Detail: se.detail}
		case errors.As(err, &je):
			bd = report.BuildDone{Reason: "judge_error", Detail: je.detail}
		default:
			bd = report.BuildDone{Reason: "internal", Detail: err.Error()}
		}
	}
	bd.ProblemID, bd.Version = j.Problem.ID, j.Problem.Version
	label := "ok"
	if !bd.OK {
		label = bd.Reason
	}
	d.Metrics.JobsTotal.WithLabelValues(j.Lane, "build", label).Inc()
	d.Metrics.JobDuration.WithLabelValues(j.Lane, "build").Observe(time.Since(start).Seconds())
	o := Outcome{DoneSent: true, Reason: bd.Reason}
	o.DoneOut = rep.Emit(ctx, "build_done", bd)
	if o.DoneOut == report.Superseded {
		return Outcome{Superseded: true}
	}
	log.Info("build done", "ok", bd.OK, "reason", bd.Reason, "answers", len(bd.Answers), "max_time_ms", bd.MaxTimeMs)
	return o
}

func (jc *jobCtx) build(ctx context.Context) (report.BuildDone, error) {
	j := jc.job
	if err := jc.ensurePackage(ctx, testdata.ValidateOpts{RequireAnswers: false}); err != nil {
		return report.BuildDone{}, err
	}
	p := jc.pkg.Problem
	if p.Type != testdata.Batch {
		return report.BuildDone{}, sysErr("unsupported_problem_type", "build jobs support batch problems only, got %s", p.Type)
	}
	l, ok := jc.d.Langs.Get(j.Reference.Language)
	if !ok {
		return report.BuildDone{}, sysErr("unknown_language", "%s", j.Reference.Language)
	}
	jc.lang = l
	files, err := jc.downloadFiles(ctx, j.Reference.Files, maxSourceBytes)
	if err != nil {
		return report.BuildDone{}, err
	}
	jc.sources = files
	co, err := jc.compileSubmission(ctx, l, j.Reference.Files)
	if err != nil {
		return report.BuildDone{}, err
	}
	if !co.OK {
		return report.BuildDone{OK: false, Reason: "reference_ce", Log: co.Log}, nil
	}
	jc.binary = co.FileID

	var answers []report.Answer
	var maxTime uint64
	for _, tc := range p.Order() {
		url, ok := j.AnswersUpload[tc+".ans"]
		if !ok {
			return report.BuildDone{}, sysErr("missing_upload_url", "%s.ans", tc)
		}
		cr, err := jc.runBatchCase(ctx, tc)
		if err != nil {
			return report.BuildDone{}, err
		}
		if t := verdict.TimeMs(cr.res); t > maxTime {
			maxTime = t
		}
		if cr.mapping.Verdict != verdict.Pending {
			jc.dropFile(ctx, cr.stdoutID)
			return report.BuildDone{OK: false, Reason: "reference_" + strings.ToLower(string(cr.mapping.Verdict)), Testcase: tc, MaxTimeMs: maxTime}, nil
		}
		path, err := jc.fetchOutput(ctx, cr.stdoutID, "ans-"+tc)
		jc.dropFile(ctx, cr.stdoutID)
		if err != nil {
			return report.BuildDone{}, err
		}
		a, err := jc.upload(ctx, path, url)
		_ = os.Remove(path)
		if err != nil {
			return report.BuildDone{}, err
		}
		a.Name = tc + ".ans"
		answers = append(answers, a)
	}
	return report.BuildDone{OK: true, Answers: answers, MaxTimeMs: maxTime}, nil
}

// upload PUTs a file to a presigned URL and returns its hash and size.
func (jc *jobCtx) upload(ctx context.Context, path, url string) (report.Answer, error) {
	f, err := os.Open(path)
	if err != nil {
		return report.Answer{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return report.Answer{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	var lastErr error
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(time.Duration(try) * time.Second)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return report.Answer{}, err
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		req, _ := http.NewRequestWithContext(rctx, http.MethodPut, url, io.NopCloser(f))
		req.ContentLength = n
		req.Header.Set("Content-Type", "text/plain")
		resp, err := jc.d.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				cancel()
				return report.Answer{SHA256: sum, Bytes: n}, nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				cancel()
				return report.Answer{}, sysErr("answer_upload_failed", "%v", err)
			}
		}
		cancel()
		lastErr = err
	}
	return report.Answer{}, &transientError{reason: "storage_unavailable", err: lastErr}
}
