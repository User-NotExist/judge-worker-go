package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setup(t *testing.T) (*miniredis.Miniredis, *redis.Client, *Consumer) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.Unix(1_700_000_000, 0))
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	c := New(rdb, Options{Group: "workers", Lanes: []string{"exam", "homework", "practice"},
		ReclaimMinIdle: time.Minute, ReclaimEvery: time.Second, StreamsTTL: time.Nanosecond})
	return mr, rdb, c
}

func enqueue(t *testing.T, rdb *redis.Client, lane, course, data string) {
	t.Helper()
	ctx := context.Background()
	stream := "judge:" + lane + ":" + course
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{"data": data}}).Err(); err != nil {
		t.Fatal(err)
	}
	rdb.SAdd(ctx, "judge:streams:"+lane, stream)
}

func next(t *testing.T, c *Consumer) *Message {
	t.Helper()
	m, err := c.Next(context.Background(), "w/slot-0")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLanePriority(t *testing.T) {
	_, rdb, c := setup(t)
	enqueue(t, rdb, "practice", "1", "p1")
	enqueue(t, rdb, "homework", "1", "h1")
	enqueue(t, rdb, "exam", "2", "e1")
	enqueue(t, rdb, "exam", "2", "e2")
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, string(next(t, c).Data))
	}
	want := []string{"e1", "e2", "h1", "p1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if m := next(t, c); m != nil {
		t.Fatal("expected empty")
	}
}

func TestRoundRobinAcrossCourses(t *testing.T) {
	_, rdb, c := setup(t)
	for i := 0; i < 3; i++ {
		enqueue(t, rdb, "homework", "A", "a")
	}
	for i := 0; i < 3; i++ {
		enqueue(t, rdb, "homework", "B", "b")
	}
	var got string
	for i := 0; i < 6; i++ {
		got += string(next(t, c).Data)
	}
	if got != "ababab" {
		t.Fatalf("got %s", got)
	}
}

func TestAckDeletes(t *testing.T) {
	_, rdb, c := setup(t)
	enqueue(t, rdb, "exam", "1", "x")
	m := next(t, c)
	if m.Deliveries != 1 || m.Lane != "exam" {
		t.Fatalf("%+v", m)
	}
	if err := c.Ack(context.Background(), m.Stream, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.XLen(context.Background(), m.Stream).Result(); n != 0 {
		t.Fatal("XDEL not applied")
	}
}

func TestReclaimDeadWorker(t *testing.T) {
	mr, rdb, c := setup(t)
	enqueue(t, rdb, "exam", "1", "x")
	m := next(t, c) // slot-0 takes it and "dies"
	ctx := context.Background()

	// Not idle long enough yet.
	got, err := c.Reclaim(ctx, "w2/slot-0")
	if err != nil || got != nil {
		t.Fatal(got, err)
	}
	advance(mr, 2*time.Minute)
	got, err = c.Reclaim(ctx, "w2/slot-0")
	if err != nil || got == nil {
		t.Fatal("expected reclaim", err)
	}
	if got.ID != m.ID || !got.Reclaimed || got.Deliveries < 2 {
		t.Fatalf("%+v", got)
	}
}

func TestHeartbeatPreventsReclaim(t *testing.T) {
	mr, rdb, c := setup(t)
	enqueue(t, rdb, "exam", "1", "x")
	m := next(t, c)
	ctx := context.Background()
	advance(mr, 50*time.Second)
	if err := c.Heartbeat(ctx, m.Stream, m.ID, "w/slot-0"); err != nil {
		t.Fatal(err)
	}
	advance(mr, 50*time.Second)
	if got, _ := c.Reclaim(ctx, "w2/slot-0"); got != nil {
		t.Fatal("heartbeat should have reset idle time")
	}
}

func TestReclaimDueOnce(t *testing.T) {
	_, _, c := setup(t)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	if !c.ReclaimDue() {
		t.Fatal("first call is due")
	}
	if c.ReclaimDue() {
		t.Fatal("second call within interval")
	}
	now = now.Add(2 * time.Second)
	if !c.ReclaimDue() {
		t.Fatal("due after interval")
	}
}

func TestLaneOf(t *testing.T) {
	if LaneOf("judge:build:_") != "build" || LaneOf("judge:exam:12") != "exam" || LaneOf("x") != "" {
		t.Fatal("LaneOf")
	}
}

var clock = map[*miniredis.Miniredis]time.Time{}

func advance(mr *miniredis.Miniredis, d time.Duration) {
	if _, ok := clock[mr]; !ok {
		clock[mr] = time.Unix(1_700_000_000, 0)
	}
	clock[mr] = clock[mr].Add(d)
	mr.SetTime(clock[mr])
}
