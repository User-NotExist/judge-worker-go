package report

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeAPI struct {
	mu     sync.Mutex
	down   atomic.Bool
	status atomic.Int32
	seqs   []int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		w.WriteHeader(503)
		return
	}
	if s := f.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	b, _ := io.ReadAll(r.Body)
	var evs []Event
	if len(b) > 0 && b[0] == '[' {
		_ = json.Unmarshal(b, &evs)
	} else {
		var e Event
		_ = json.Unmarshal(b, &e)
		evs = []Event{e}
	}
	f.mu.Lock()
	for _, e := range evs {
		f.seqs = append(f.seqs, e.Seq)
	}
	f.mu.Unlock()
	w.WriteHeader(200)
}

type fakeAcker struct{ acks, touches atomic.Int32 }

func (a *fakeAcker) Ack(context.Context, string, string) error   { a.acks.Add(1); return nil }
func (a *fakeAcker) Touch(context.Context, string, string) error { a.touches.Add(1); return nil }

func TestSpoolAndReplayInOrder(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := &Client{URL: srv.URL, HTTP: srv.Client()}
	sp := &Spool{Dir: t.TempDir()}
	_ = sp.Init()
	acker := &fakeAcker{}
	rp := &Replayer{Spool: sp, Client: cl, Acker: acker, Log: log}
	rep := &Reporter{Client: cl, Spool: sp, Log: log, SubmissionID: 1, Attempt: 2, JobID: "j", WorkerID: "w"}
	ctx := context.Background()

	if o := rep.Emit(ctx, "started", Started{}); o != Accepted {
		t.Fatal(o)
	}
	api.down.Store(true)
	if o := rep.Emit(ctx, "test_result", TestResult{}); o != Spooled {
		t.Fatal(o)
	}
	api.down.Store(false)
	// Sticky: once spooled, later events go to the spool too, preserving order.
	if o := rep.Emit(ctx, "done", Done{}); o != Spooled {
		t.Fatal(o)
	}
	_ = rep.DeferAck("s", "1-0")
	if acker.acks.Load() != 0 {
		t.Fatal("must not ack before done is accepted")
	}
	rp.Pass(ctx)
	if acker.acks.Load() != 1 {
		t.Fatal("ack after replay")
	}
	if len(api.seqs) != 3 || api.seqs[0] != 1 || api.seqs[1] != 2 || api.seqs[2] != 3 {
		t.Fatal(api.seqs)
	}
	by, _ := rp.list()
	if len(by) != 0 {
		t.Fatal("spool not empty")
	}
}

func TestReplayFailureKeepsFilesAndTouches(t *testing.T) {
	api := &fakeAPI{}
	api.down.Store(true)
	srv := httptest.NewServer(api)
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := &Client{URL: srv.URL, HTTP: srv.Client()}
	sp := &Spool{Dir: t.TempDir()}
	_ = sp.Init()
	acker := &fakeAcker{}
	rp := &Replayer{Spool: sp, Client: cl, Acker: acker, Log: log}
	ev := &Event{Seq: 1, EventID: "1:1:1"}
	_ = sp.AppendEvent("1:1", ev)
	_ = sp.AppendAck("1:1", AckRef{"s", "1-0"})
	rp.Pass(context.Background())
	if acker.acks.Load() != 0 || acker.touches.Load() == 0 {
		t.Fatal("expected touch, no ack")
	}
	// New events appended while the first file is in flight replay after it.
	_ = sp.AppendEvent("1:1", &Event{Seq: 2, EventID: "1:1:2"})
	api.down.Store(false)
	rp.Pass(context.Background())
	if len(api.seqs) != 2 || api.seqs[0] != 1 || api.seqs[1] != 2 || acker.acks.Load() != 1 {
		t.Fatal(api.seqs, acker.acks.Load())
	}
}

func TestOutcomes(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl := &Client{URL: srv.URL, HTTP: srv.Client()}
	rep := &Reporter{Client: cl, Spool: &Spool{Dir: t.TempDir()}, Log: log, SubmissionID: 1, Attempt: 1, JobID: "j"}
	api.status.Store(409)
	if o := rep.Emit(context.Background(), "done", Done{}); o != Superseded {
		t.Fatal(o)
	}
	api.status.Store(400)
	if o := rep.Emit(context.Background(), "done", Done{}); o != Dropped {
		t.Fatal(o)
	}
}
