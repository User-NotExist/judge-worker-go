package report

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Reporter emits the events of one job attempt with a monotonically increasing seq.
type Reporter struct {
	Client   *Client
	Spool    *Spool
	Replayer *Replayer
	Log      *slog.Logger

	SubmissionID int64
	Attempt      int
	JobID        string
	WorkerID     string
	OnDropped    func()
	OnSpooled    func()

	mu       sync.Mutex
	seq      int
	spooling bool
}

func (r *Reporter) key() string { return EventKey(r.SubmissionID, r.JobID, r.Attempt) }

// Emit posts one event: up to 3 tries, then spool (§12).
func (r *Reporter) Emit(ctx context.Context, typ string, payload any) Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	pb, err := json.Marshal(payload)
	if err != nil {
		r.Log.Error("marshal event", "type", typ, "err", err)
		return Dropped
	}
	r.seq++
	ev := &Event{
		EventID:      fmt.Sprintf("%s:%d", r.key(), r.seq),
		Seq:          r.seq,
		Type:         typ,
		SubmissionID: r.SubmissionID,
		Attempt:      r.Attempt,
		JobID:        r.JobID,
		WorkerID:     r.WorkerID,
		SentAt:       time.Now().UTC(),
		Payload:      pb,
	}
	if !r.spooling {
		o, err := r.Client.PostWithRetry(ctx, ev, 3)
		if err == nil || !IsRetryable(err) {
			if o == Dropped {
				r.Log.Error("event rejected by API; dropped", "type", typ, "seq", r.seq, "err", err)
				if r.OnDropped != nil {
					r.OnDropped()
				}
			}
			return o
		}
		r.Log.Warn("events API unreachable; spooling", "type", typ, "seq", r.seq, "err", err)
		r.spooling = true
	}
	if err := r.Spool.AppendEvent(r.key(), ev); err != nil {
		r.Log.Error("spool write failed; event lost", "type", typ, "seq", r.seq, "err", err)
		return Dropped
	}
	if r.OnSpooled != nil {
		r.OnSpooled()
	}
	return Spooled
}

// DeferAck records that the Redis message must be acked once the spooled `done` is
// accepted. Only valid after Emit returned Spooled.
func (r *Reporter) DeferAck(stream, id string) error {
	err := r.Spool.AppendAck(r.key(), AckRef{Stream: stream, ID: id})
	if r.Replayer != nil {
		r.Replayer.Kick()
	}
	return err
}
