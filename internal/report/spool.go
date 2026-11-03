package report

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Spool is an append-only JSON-lines file per job attempt, fsync'd per write (§12).
//
// File states, per key:
//
//	<key>.jsonl           written by the running job
//	<key>.jsonl.inflight  claimed by the replayer (renamed atomically under mu)
//
// The replayer only claims <key>.jsonl when no .inflight exists for that key, so events
// are always replayed in seq order. A job that has spooled once keeps spooling (sticky),
// so live posts never overtake spooled ones.
type Spool struct {
	Dir string
	mu  sync.Mutex
}

type line struct {
	Event *Event  `json:"event,omitempty"`
	Ack   *AckRef `json:"ack,omitempty"`
}

func safeKey(k string) string { return strings.NewReplacer(":", "_", "/", "_").Replace(k) }

func (s *Spool) path(key string) string { return filepath.Join(s.Dir, safeKey(key)+".jsonl") }

func (s *Spool) Init() error { return os.MkdirAll(s.Dir, 0o750) }

func (s *Spool) append(key string, l line) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path(key), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Spool) AppendEvent(key string, e *Event) error { return s.append(key, line{Event: e}) }
func (s *Spool) AppendAck(key string, a AckRef) error   { return s.append(key, line{Ack: &a}) }

// Acker is called by the replayer once a spooled `done` is accepted (or dropped).
type Acker interface {
	Ack(ctx context.Context, stream, id string) error
	Touch(ctx context.Context, stream, id string) error // heartbeat (XCLAIM JUSTID)
}

type Replayer struct {
	Spool     *Spool
	Client    *Client
	Acker     Acker
	Log       *slog.Logger
	Heartbeat time.Duration
	OnGauge   func(n int)
	OnDropped func()

	kick     chan struct{}
	failures int
	lastBeat time.Time
}

func (r *Replayer) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *Replayer) Run(ctx context.Context) {
	r.kick = make(chan struct{}, 1)
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
		delay := r.Pass(ctx)
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(delay)
	}
}

type spoolFile struct {
	key      string
	path     string
	inflight bool
}

func (r *Replayer) list() (map[string][]spoolFile, []string) {
	ents, _ := os.ReadDir(r.Spool.Dir)
	by := map[string][]spoolFile{}
	for _, e := range ents {
		n := e.Name()
		p := filepath.Join(r.Spool.Dir, n)
		switch {
		case strings.HasSuffix(n, ".jsonl.inflight"):
			k := strings.TrimSuffix(n, ".jsonl.inflight")
			by[k] = append([]spoolFile{{k, p, true}}, by[k]...)
		case strings.HasSuffix(n, ".jsonl"):
			k := strings.TrimSuffix(n, ".jsonl")
			by[k] = append(by[k], spoolFile{k, p, false})
		}
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return by, keys
}

// Pass replays everything once and returns the delay until the next pass.
func (r *Replayer) Pass(ctx context.Context) time.Duration {
	by, keys := r.list()
	pending := 0
	failed := false
	beat := r.Heartbeat > 0 && time.Since(r.lastBeat) >= r.Heartbeat
	if beat {
		r.lastBeat = time.Now()
	}
	for _, k := range keys {
		files := by[k]
		if failed {
			pending += r.count(files, beat, ctx)
			continue
		}
		for _, f := range files {
			if !f.inflight {
				// claim it (only reached when no inflight exists or it was just finished)
				r.Spool.mu.Lock()
				inflight := f.path + ".inflight"
				err := os.Rename(f.path, inflight)
				r.Spool.mu.Unlock()
				if err != nil {
					continue
				}
				f.path, f.inflight = inflight, true
			}
			n, ok := r.replayFile(ctx, f.path)
			if !ok {
				pending += n
				failed = true
				break
			}
		}
	}
	if r.OnGauge != nil {
		r.OnGauge(pending)
	}
	if failed {
		r.failures++
		return Backoff(r.failures)
	}
	r.failures = 0
	if pending > 0 {
		return time.Second
	}
	return 5 * time.Second
}

func (r *Replayer) read(path string) ([]*Event, []AckRef) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	var evs []*Event
	var acks []AckRef
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // torn last line after a crash
		}
		if l.Event != nil {
			evs = append(evs, l.Event)
		}
		if l.Ack != nil {
			acks = append(acks, *l.Ack)
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	return evs, acks
}

// count reports pending events and keeps their Redis messages alive.
func (r *Replayer) count(files []spoolFile, beat bool, ctx context.Context) int {
	n := 0
	for _, f := range files {
		evs, acks := r.read(f.path)
		n += len(evs)
		if beat {
			for _, a := range acks {
				_ = r.Acker.Touch(ctx, a.Stream, a.ID)
			}
		}
	}
	return n
}

// replayFile posts the file's events in batches of 50. Returns (pending, ok).
func (r *Replayer) replayFile(ctx context.Context, path string) (int, bool) {
	evs, acks := r.read(path)
	outcome := Accepted
	for i := 0; i < len(evs); i += 50 {
		batch := evs[i:min(i+50, len(evs))]
		var body any = batch
		if len(batch) == 1 {
			body = batch[0]
		}
		o, err := r.Client.post(ctx, body)
		if err != nil && IsRetryable(err) {
			r.Log.Warn("spool replay failed", "file", filepath.Base(path), "err", err)
			for _, a := range acks {
				_ = r.Acker.Touch(ctx, a.Stream, a.ID)
			}
			return len(evs) - i, false
		}
		if o == Dropped {
			r.Log.Error("spooled events rejected by API; dropped", "file", filepath.Base(path), "err", err)
			if r.OnDropped != nil {
				r.OnDropped()
			}
		}
		if o == Superseded {
			outcome = Superseded
			break
		}
	}
	if outcome != Superseded {
		for _, a := range acks {
			if err := r.Acker.Ack(ctx, a.Stream, a.ID); err != nil {
				r.Log.Warn("ack after replay failed", "err", err)
				return 0, false
			}
		}
	}
	_ = os.Remove(path)
	if len(evs) > 0 {
		r.Log.Info("spool replayed", "file", filepath.Base(path), "events", len(evs), "outcome", outcome.String())
	}
	return 0, true
}
