package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"judge-worker/internal/config"
	"judge-worker/internal/gojudge"
	"judge-worker/internal/health"
	"judge-worker/internal/job"
	"judge-worker/internal/metrics"
	"judge-worker/internal/pipeline"
	"judge-worker/internal/queue"
	"judge-worker/internal/report"
	"judge-worker/internal/verdict"
)

// presignedTTL: reclaimed jobs older than this get fresh URLs from the API (§6.1).
const presignedTTL = 25 * time.Minute // 30 min expiry minus a safety margin

type Worker struct {
	cfg      *config.Config
	log      *slog.Logger
	deps     *pipeline.Deps
	consumer *queue.Consumer
	client   *report.Client
	spool    *report.Spool
	replayer *report.Replayer
	health   *health.Checker
	m        *metrics.M
	apiHTTP  *http.Client

	paused     atomic.Bool // go-judge down or disk full: stop pulling
	pauseOnce  sync.Mutex
	pausing    bool
	healthKick []chan struct{}

	slotMu      sync.Mutex
	slotHealthy []bool
	busy        atomic.Int64
}

func (w *Worker) consumerName(slot int) string {
	return fmt.Sprintf("%s/slot-%d", w.cfg.WorkerID, slot)
}

func (w *Worker) setSlotHealthy(slot int, ok bool) {
	w.slotMu.Lock()
	w.slotHealthy[slot] = ok
	w.slotMu.Unlock()
	v := 0.0
	if ok {
		v = 1
	}
	w.m.Slot(slot).Set(v)
}

// Healthy: at least one slot can take jobs (used by /healthz and `judge-worker healthcheck`).
func (w *Worker) Healthy() bool {
	w.slotMu.Lock()
	defer w.slotMu.Unlock()
	for _, ok := range w.slotHealthy {
		if ok {
			return true
		}
	}
	return false
}

// KickHealth runs a health check on every slot at its next opportunity (SIGUSR1).
func (w *Worker) KickHealth() {
	for _, ch := range w.healthKick {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// pauseForSandbox stops pulling until go-judge /version succeeds continuously for 60 s (§14).
func (w *Worker) pauseForSandbox(ctx context.Context) {
	w.pauseOnce.Lock()
	if w.pausing {
		w.pauseOnce.Unlock()
		return
	}
	w.pausing = true
	w.paused.Store(true)
	w.pauseOnce.Unlock()
	w.log.Error("go-judge unavailable; stop pulling jobs")
	go func() {
		defer func() {
			w.pauseOnce.Lock()
			w.pausing = false
			w.paused.Store(false)
			w.pauseOnce.Unlock()
		}()
		var okSince time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			if _, err := w.deps.GJ.Version(ctx); err != nil {
				okSince = time.Time{}
				continue
			}
			if okSince.IsZero() {
				okSince = time.Now()
			}
			if time.Since(okSince) >= 60*time.Second {
				w.log.Info("go-judge healthy for 60 s; resume pulling")
				return
			}
		}
	}()
}

func (w *Worker) pauseForDisk(ctx context.Context) {
	if !w.paused.CompareAndSwap(false, true) {
		return
	}
	w.m.Alert("disk_full")
	w.log.Error("disk full; stop pulling and evict cache")
	go func() {
		defer w.paused.Store(false)
		w.deps.Cache.EvictTo(w.cfg.Cache.MaxBytes.U() / 2)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}()
}

// runSlot is the per-slot loop (§7.1). pullCtx stops pulling; jobCtx cancels running jobs.
func (w *Worker) runSlot(pullCtx, jobCtx context.Context, slot int, initialHealthy bool) {
	log := w.log.With("slot", slot)
	consumer := w.consumerName(slot)
	lastCheck := time.Now()
	healthy := initialHealthy
	w.setSlotHealthy(slot, healthy)
	consecutiveSE := 0
	idle := w.cfg.Redis.IdlePoll.D()

	sleep := func(d time.Duration) bool {
		select {
		case <-pullCtx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	check := func() {
		ctx, cancel := context.WithTimeout(jobCtx, 3*time.Minute)
		err := w.health.Quick(ctx)
		cancel()
		lastCheck = time.Now()
		if err != nil {
			if healthy {
				w.m.Alert("slot_unhealthy")
			}
			log.Error("slot health check failed; quarantined", "err", err)
			healthy = false
		} else {
			if !healthy {
				log.Info("slot health check passed; slot enabled")
			}
			healthy = true
			consecutiveSE = 0
		}
		w.setSlotHealthy(slot, healthy)
	}

	for pullCtx.Err() == nil {
		select {
		case <-w.healthKick[slot]:
			check()
			continue
		default:
		}
		if !healthy {
			if time.Since(lastCheck) >= time.Minute {
				check()
			} else if !sleep(time.Second) {
				return
			}
			continue
		}
		if time.Since(lastCheck) >= w.cfg.Health.Interval.D() {
			check()
			continue
		}
		if w.paused.Load() {
			if !sleep(time.Second) {
				return
			}
			continue
		}

		var msg *queue.Message
		var err error
		if w.consumer.ReclaimDue() {
			msg, err = w.consumer.Reclaim(pullCtx, consumer)
		}
		if msg == nil && err == nil {
			msg, err = w.consumer.Next(pullCtx, consumer)
		}
		if err != nil {
			if pullCtx.Err() != nil {
				return
			}
			log.Error("redis read failed", "err", err)
			if !sleep(5 * idle) {
				return
			}
			continue
		}
		if msg == nil {
			if !sleep(idle) {
				return
			}
			continue
		}

		w.busy.Add(1)
		w.m.BusySlots.Set(float64(w.busy.Load()))
		o := w.handle(jobCtx, slot, consumer, msg, log)
		w.m.BusySlots.Set(float64(w.busy.Add(-1)))

		switch {
		case o.Transient && o.TransientReason == "sandbox_unavailable":
			w.m.GoJudgeErrors.WithLabelValues("transport").Inc()
			w.pauseForSandbox(jobCtx)
		case o.Transient && o.TransientReason == "disk_full":
			w.pauseForDisk(jobCtx)
		}
		if o.DoneSent && o.Verdict == verdict.SE {
			consecutiveSE++
			if consecutiveSE >= 2 {
				log.Error("slot returned SE twice in a row; quarantined")
				w.m.Alert("slot_quarantined")
				healthy = false
				lastCheck = time.Now()
				w.setSlotHealthy(slot, false)
			}
		} else if o.DoneSent {
			consecutiveSE = 0
		}
	}
}

// handle runs one message end to end and settles it in Redis.
func (w *Worker) handle(ctx context.Context, slot int, consumer string, msg *queue.Message, slotLog *slog.Logger) pipeline.Outcome {
	partial := job.ParsePartial(msg.Data)
	log := slotLog.With("stream", msg.Stream, "msg_id", msg.ID, "job_id", partial.JobID, "deliveries", msg.Deliveries)

	newReporter := func(subID int64, jobID string, attempt int) *report.Reporter {
		return &report.Reporter{
			Client: w.client, Spool: w.spool, Replayer: w.replayer, Log: log,
			SubmissionID: subID, Attempt: attempt, JobID: jobID, WorkerID: w.cfg.WorkerID,
			OnDropped: w.m.EventsDropped.Inc, OnSpooled: w.replayer.Kick,
		}
	}
	// settle acks once done is accepted (or defers to the spool).
	settle := func(rep *report.Reporter, o pipeline.Outcome) pipeline.Outcome {
		if !o.DoneSent || o.Superseded {
			return o
		}
		switch o.DoneOut {
		case report.Accepted, report.Dropped:
			if err := w.consumer.Ack(context.WithoutCancel(ctx), msg.Stream, msg.ID); err != nil {
				log.Error("XACK failed", "err", err)
			}
		case report.Spooled:
			if err := rep.DeferAck(msg.Stream, msg.ID); err != nil {
				log.Error("spool ack write failed", "err", err)
			}
		}
		return o
	}
	// fail reports done{SE, reason} for jobs that cannot be run at all.
	fail := func(reason string, attempt int) pipeline.Outcome {
		w.m.Alert(reason)
		if attempt < 1 {
			attempt = 1
		}
		rep := newReporter(partial.SubmissionID, partial.JobID, attempt)
		if partial.Kind == "build" || (partial.SubmissionID == 0 && partial.JobID != "") {
			rep.SubmissionID = 0
			out := rep.Emit(ctx, "build_done", report.BuildDone{OK: false, Reason: reason})
			return settle(rep, pipeline.Outcome{DoneSent: true, DoneOut: out, Verdict: verdict.SE, Reason: reason})
		}
		if partial.SubmissionID == 0 {
			log.Error("unparseable job without submission_id; acking", "reason", reason)
			_ = w.consumer.Ack(ctx, msg.Stream, msg.ID)
			return pipeline.Outcome{}
		}
		r := reason
		out := rep.Emit(ctx, "done", report.Done{Verdict: string(verdict.SE), Reason: &r, SubtaskScoresMilli: []int64{}, GoJudgeVersion: w.deps.GoJudgeVersion()})
		return settle(rep, pipeline.Outcome{DoneSent: true, DoneOut: out, Verdict: verdict.SE, Reason: reason})
	}

	attempt := partial.Attempt + int(msg.Deliveries) - 1
	if msg.Deliveries > 3 {
		log.Error("poison message: delivered more than 3 times")
		return fail("poison", attempt)
	}
	j, err := job.Parse(msg.Data)
	if err != nil {
		log.Error("malformed job", "err", err)
		return fail("bad_job", attempt)
	}
	j.Attempt = j.Attempt + int(msg.Deliveries) - 1
	if j.Lane == "" {
		j.Lane = msg.Lane
	}
	if msg.Reclaimed && !j.CreatedAt.IsZero() && time.Since(j.CreatedAt) > presignedTTL {
		fresh, gone, err := w.refresh(ctx, j.JobID)
		switch {
		case gone:
			log.Warn("job no longer exists at the API; acking")
			_ = w.consumer.Ack(ctx, msg.Stream, msg.ID)
			return pipeline.Outcome{}
		case err != nil:
			log.Warn("job refresh failed; leaving for reclaim", "err", err)
			return pipeline.Outcome{Transient: true, TransientReason: "refresh_failed"}
		}
		attempt := j.Attempt
		j = fresh
		if j.Attempt < attempt {
			j.Attempt = attempt
		}
		if j.Lane == "" {
			j.Lane = msg.Lane
		}
	}

	log = log.With("submission_id", j.SubmissionID, "attempt", j.Attempt, "course_id", j.CourseID, "lane", j.Lane, "kind", j.Kind)
	log.Info("job started", "problem_id", j.Problem.ID, "problem_version", j.Problem.Version, "language", j.Language, "reclaimed", msg.Reclaimed)

	// Heartbeat: keep the message's idle time low while the job runs.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(w.cfg.Redis.Heartbeat.D())
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := w.consumer.Heartbeat(hbCtx, msg.Stream, msg.ID, consumer); err != nil && hbCtx.Err() == nil {
					log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()

	var subID int64
	if j.Kind == "judge" {
		subID = j.SubmissionID
	}
	rep := newReporter(subID, j.JobID, j.Attempt)
	var o pipeline.Outcome
	if j.Kind == "build" {
		o = w.deps.RunBuild(ctx, j, slot, rep, log)
	} else {
		o = w.deps.RunJudge(ctx, j, slot, rep, log)
	}
	stopHB()
	return settle(rep, o)
}

// refresh re-requests a job with fresh presigned URLs (§6.1). gone = 404/410.
func (w *Worker) refresh(ctx context.Context, jobID string) (*job.Job, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := strings.TrimSuffix(w.cfg.API.JobsURL, "/") + "/" + jobID + "/refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.apiHTTP.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, true, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("refresh HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	// Accept either the job itself or {"job": {...}}.
	var wrapped struct {
		Job json.RawMessage `json:"job"`
	}
	if json.Unmarshal(b, &wrapped) == nil && len(wrapped.Job) > 0 {
		b = wrapped.Job
	}
	j, err := job.Parse(b)
	if err != nil {
		return nil, false, fmt.Errorf("refresh: %w", err)
	}
	return j, false, nil
}

var errStartup = errors.New("startup check failed")

// startupChecks: log go-judge version, verify parallelism == slots (§4).
func startupChecks(ctx context.Context, cfg *config.Config, gj gojudge.Client, log *slog.Logger) (string, error) {
	var ver string
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for {
		ver, err = gj.Version(ctx)
		if err == nil || time.Now().After(deadline) {
			break
		}
		log.Warn("waiting for go-judge", "err", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: go-judge /version: %v", errStartup, err)
	}
	gcfg, err := gj.Config(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: go-judge /config: %v", errStartup, err)
	}
	p := gojudge.FindParallelism(gcfg)
	switch {
	case p == 0:
		log.Warn("go-judge /config does not expose parallelism; cannot verify slots == -parallelism", "slots", cfg.Slots)
	case p != cfg.Slots:
		return "", fmt.Errorf("%w: go-judge parallelism %d != slots %d", errStartup, p, cfg.Slots)
	}
	log.Info("go-judge connected", "go_judge_version", ver, "parallelism", p)
	return ver, nil
}
