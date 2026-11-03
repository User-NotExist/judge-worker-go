package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"judge-worker/internal/job"
	"judge-worker/internal/pkgcache"
	"judge-worker/internal/report"
	"judge-worker/internal/score"
	"judge-worker/internal/verdict"
)

// Outcome tells the slot what to do with the Redis message.
type Outcome struct {
	Verdict verdict.Verdict
	Reason  string
	// DoneSent: a done/build_done event was emitted with this result.
	DoneSent bool
	DoneOut  report.Outcome
	// Transient: no done was sent; leave the message un-acked and stop heartbeating.
	Transient       bool
	TransientReason string
	// Superseded: API returned 409; stop, do not ack.
	Superseded bool
}

// RunJudge judges one submission end to end.
func (d *Deps) RunJudge(ctx context.Context, j *job.Job, slot int, rep *report.Reporter, log *slog.Logger) Outcome {
	start := time.Now()
	jc, err := d.newJobCtx(j, log, rep)
	if err != nil {
		return d.finish(ctx, nil, j, start, &transientError{reason: "disk_full", err: err}, score.Result{})
	}
	defer jc.close()

	if err := jc.emit(ctx, "started", report.Started{Slot: slot, GoJudgeVersion: d.GoJudgeVersion()}); err != nil {
		return d.finish(ctx, jc, j, start, err, score.Result{})
	}
	if q := time.Since(j.CreatedAt); !j.CreatedAt.IsZero() && q >= 0 {
		d.Metrics.QueueWait.WithLabelValues(j.Lane).Observe(q.Seconds())
	}
	res, err := jc.judge(ctx)
	return d.finish(ctx, jc, j, start, err, res)
}

func (jc *jobCtx) judge(ctx context.Context) (score.Result, error) {
	j := jc.job
	if err := jc.ensurePackage(ctx, pkgcache.ValidateOpts{Language: j.Language, RequireAnswers: true}); err != nil {
		return score.Result{}, err
	}
	p := jc.pkg.Problem
	var runner score.Runner

	switch p.Type {
	case pkgcache.OutputOnly:
		if j.Language != job.OutputOnly {
			return score.Result{}, sysErr("bad_job", "output_only problem judged as %s", j.Language)
		}
		files, err := jc.downloadFiles(ctx, j.Source.Files, 64<<20)
		if err != nil {
			return score.Result{}, err
		}
		jc.sources = files
		runner = jc.outputOnlyCase(ctx)

	case pkgcache.Batch, pkgcache.Interactive:
		if j.Language == job.OutputOnly {
			return score.Result{}, sysErr("bad_job", "language output-only for %s problem", p.Type)
		}
		l, ok := jc.d.Langs.Get(j.Language)
		if !ok {
			jc.d.Metrics.Alert("unknown_language")
			return score.Result{}, sysErr("unknown_language", "%s", j.Language)
		}
		jc.lang = l
		files, err := jc.downloadFiles(ctx, j.Source.Files, maxSourceBytes)
		if err != nil {
			return score.Result{}, err
		}
		jc.sources = files
		co, err := jc.compileSubmission(ctx, l, j.Source.Files)
		if err != nil {
			return score.Result{}, err
		}
		if err := jc.emit(ctx, "compiled", report.Compiled{OK: co.OK, TimeMs: co.TimeMs, Log: co.Log, Truncated: co.Truncated}); err != nil {
			return score.Result{}, err
		}
		if !co.OK {
			return score.Result{Verdict: verdict.CE, SubtaskMilli: make([]int64, len(p.Subtasks))}, errCompile
		}
		jc.binary = co.FileID
		if p.Type == pkgcache.Batch {
			runner = jc.batchCase(ctx)
		} else {
			runner = jc.interactiveCase(ctx)
		}
	}

	return score.Score(p.ScoreSubtasks(), p.Order(), runner, score.Hooks{
		Skipped: func(st int, tcs []string) {
			_ = jc.emit(ctx, "skipped", report.Skipped{Subtask: st, Testcases: tcs})
		},
	})
}

var errCompile = errors.New("compile error")

// observe wraps a runner with the per-testcase metric and the test_result event.
func (jc *jobCtx) caseDone(ctx context.Context, start time.Time, tr report.TestResult, v verdict.Verdict, frac float64) (score.CaseResult, error) {
	langLabel := jc.job.Language
	jc.d.Metrics.TestcaseDuration.WithLabelValues(langLabel).Observe(time.Since(start).Seconds())
	if err := jc.emit(ctx, "test_result", tr); err != nil {
		return score.CaseResult{}, err
	}
	return score.CaseResult{Verdict: v, Fraction: frac}, nil
}

func (jc *jobCtx) batchCase(ctx context.Context) score.Runner {
	return func(tc string) (score.CaseResult, error) {
		start := time.Now()
		cr, err := jc.runBatchCase(ctx, tc)
		if err != nil {
			return score.CaseResult{}, err
		}
		defer jc.dropFile(ctx, cr.stdoutID)
		if cr.mapping.Verdict != verdict.Pending {
			tr := jc.testResult(tc, cr.mapping.Verdict, 0, cr.mapping.Message, &cr.res)
			return jc.caseDone(ctx, start, tr, cr.mapping.Verdict, 0)
		}
		// Fetch the output locally once when the default checker or a preview needs it.
		var outPath string
		if !jc.pkg.Problem.HasChecker() || jc.previewAllowed(tc) {
			outPath, err = jc.fetchOutput(ctx, cr.stdoutID, "out-"+tc)
			if err != nil {
				return score.CaseResult{}, err
			}
			defer os.Remove(outPath)
		}
		c, err := jc.check(ctx, tc, output{fileID: cr.stdoutID, has: true}, outPath)
		if err != nil {
			return score.CaseResult{}, err
		}
		tr := jc.testResult(tc, c.Verdict, c.Fraction, c.Message, &cr.res)
		if jc.previewAllowed(tc) {
			if f, err := os.Open(outPath); err == nil {
				tr.OutputPreview, tr.PreviewTruncated = jc.previewFrom(f)
				f.Close()
			}
		}
		return jc.caseDone(ctx, start, tr, c.Verdict, c.Fraction)
	}
}

func (jc *jobCtx) outputOnlyCase(ctx context.Context) score.Runner {
	return func(tc string) (score.CaseResult, error) {
		start := time.Now()
		b, ok := jc.sources[tc+".out"]
		if !ok {
			tr := jc.testResult(tc, verdict.WA, 0, "no output submitted for this testcase", nil)
			return jc.caseDone(ctx, start, tr, verdict.WA, 0)
		}
		out := output{inline: b, has: true}
		if jc.pkg.Problem.HasChecker() && len(b) > 1<<20 {
			id, err := jc.d.GJ.UploadFile(ctx, tc+".out", bytesReader(b))
			if err != nil {
				return score.CaseResult{}, &transientError{reason: "sandbox_unavailable", err: err}
			}
			jc.fileIDs[id] = true
			out = output{fileID: id, has: true}
			defer jc.dropFile(ctx, id)
		}
		c, err := jc.check(ctx, tc, out, "")
		if err != nil {
			return score.CaseResult{}, err
		}
		tr := jc.testResult(tc, c.Verdict, c.Fraction, c.Message, nil)
		if jc.previewAllowed(tc) {
			tr.OutputPreview, tr.PreviewTruncated = jc.previewFrom(bytesReader(b))
		}
		return jc.caseDone(ctx, start, tr, c.Verdict, c.Fraction)
	}
}

// finish converts the job's error into the final event and outcome.
func (d *Deps) finish(ctx context.Context, jc *jobCtx, j *job.Job, start time.Time, err error, res score.Result) Outcome {
	kind := j.Kind
	log := slog.Default()
	if jc != nil {
		log = jc.log
	}
	var te *transientError
	switch {
	case errors.Is(err, errSuperseded):
		log.Warn("attempt superseded; stopping job")
		return Outcome{Superseded: true}
	case errors.As(err, &te):
		log.Warn("job interrupted by infrastructure failure; leaving message for reclaim", "reason", te.reason, "err", te.err)
		return Outcome{Transient: true, TransientReason: te.reason}
	case err != nil && ctx.Err() != nil && !errors.Is(err, errCompile):
		log.Warn("job cancelled", "err", err)
		return Outcome{Transient: true, TransientReason: "cancelled"}
	}

	done := report.Done{
		GoJudgeVersion: d.GoJudgeVersion(),
		Toolchain:      d.Toolchain(j.Language),
		DurationMs:     time.Since(start).Milliseconds(),
	}
	var se *sysError
	var je *judgeError
	switch {
	case err == nil:
		done.Verdict = string(res.Verdict)
		done.ScoreMilli = &res.TotalMilli
		done.SubtaskScoresMilli = res.SubtaskMilli
	case errors.Is(err, errCompile):
		zero := int64(0)
		done.Verdict = string(verdict.CE)
		done.ScoreMilli = &zero
		done.SubtaskScoresMilli = res.SubtaskMilli
	case errors.As(err, &je):
		done.Verdict = string(verdict.JE)
		reason := "judge_error"
		done.Reason = &reason
		done.Detail = je.detail
	case errors.As(err, &se):
		done.Verdict = string(verdict.SE)
		done.Reason = &se.reason
		done.Detail = se.detail
	default:
		done.Verdict = string(verdict.SE)
		reason := "internal"
		done.Reason = &reason
		done.Detail = err.Error()
	}
	if done.SubtaskScoresMilli == nil {
		done.SubtaskScoresMilli = []int64{}
	}
	if err != nil && !errors.Is(err, errCompile) {
		log.Error("job failed", "verdict", done.Verdict, "reason", *done.Reason, "detail", done.Detail)
	}
	d.Metrics.JobsTotal.WithLabelValues(j.Lane, kind, done.Verdict).Inc()
	d.Metrics.JobDuration.WithLabelValues(j.Lane, kind).Observe(time.Since(start).Seconds())

	o := Outcome{Verdict: verdict.Verdict(done.Verdict), DoneSent: true}
	if done.Reason != nil {
		o.Reason = *done.Reason
	}
	if jc == nil || jc.rep == nil {
		return o
	}
	o.DoneOut = jc.rep.Emit(ctx, "done", done)
	if o.DoneOut == report.Superseded {
		return Outcome{Superseded: true}
	}
	log.Info("job done", "verdict", done.Verdict, "score_milli", done.ScoreMilli, "duration_ms", done.DurationMs, "event", o.DoneOut.String())
	return o
}
