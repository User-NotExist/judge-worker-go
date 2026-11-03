// Package queue consumes jobs from Redis Streams with strict lane priority, per-course
// round-robin fairness, heartbeats and reclaim of dead workers' messages (§7).
package queue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

type Message struct {
	Stream     string
	ID         string
	Lane       string
	Data       []byte
	Deliveries int64
	Reclaimed  bool
}

type Options struct {
	Group          string
	Lanes          []string
	ReclaimMinIdle time.Duration
	ReclaimEvery   time.Duration
	StreamsTTL     time.Duration // SMEMBERS cache; 5 s
}

type Consumer struct {
	rdb  redis.UniversalClient
	opts Options
	now  func() time.Time

	mu       sync.Mutex
	streams  map[string]cachedStreams
	rr       map[string]int // lane -> next start index
	groupsOK map[string]bool

	lastReclaim  atomic.Int64 // unix nano
	reclaimAgain atomic.Bool
}

type cachedStreams struct {
	list    []string
	fetched time.Time
}

func New(rdb redis.UniversalClient, o Options) *Consumer {
	if o.StreamsTTL == 0 {
		o.StreamsTTL = 5 * time.Second
	}
	return &Consumer{
		rdb: rdb, opts: o, now: time.Now,
		streams: map[string]cachedStreams{}, rr: map[string]int{}, groupsOK: map[string]bool{},
	}
}

func LaneOf(stream string) string {
	parts := strings.SplitN(stream, ":", 3)
	if len(parts) == 3 && parts[0] == "judge" {
		return parts[1]
	}
	return ""
}

func (c *Consumer) laneStreams(ctx context.Context, lane string) ([]string, error) {
	c.mu.Lock()
	cs, ok := c.streams[lane]
	c.mu.Unlock()
	if ok && c.now().Sub(cs.fetched) < c.opts.StreamsTTL {
		return cs.list, nil
	}
	list, err := c.rdb.SMembers(ctx, "judge:streams:"+lane).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(list) // stable base order; rotation provides fairness
	c.mu.Lock()
	c.streams[lane] = cachedStreams{list: list, fetched: c.now()}
	c.mu.Unlock()
	return list, nil
}

// rotation returns streams starting at the lane's round-robin index and advances it.
func (c *Consumer) rotation(lane string, list []string) []string {
	if len(list) == 0 {
		return nil
	}
	c.mu.Lock()
	start := c.rr[lane] % len(list)
	c.rr[lane] = (start + 1) % len(list)
	c.mu.Unlock()
	out := make([]string, 0, len(list))
	out = append(out, list[start:]...)
	return append(out, list[:start]...)
}

func isNoGroup(err error) bool { return err != nil && strings.HasPrefix(err.Error(), "NOGROUP") }

func (c *Consumer) ensureGroup(ctx context.Context, stream string) error {
	err := c.rdb.XGroupCreate(ctx, stream, c.opts.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	c.mu.Lock()
	c.groupsOK[stream] = true
	c.mu.Unlock()
	return nil
}

func (c *Consumer) readOne(ctx context.Context, stream, consumer string) (*Message, error) {
	for try := 0; try < 2; try++ {
		res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: c.opts.Group, Consumer: consumer, Streams: []string{stream, ">"}, Count: 1, Block: -1,
		}).Result()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		if isNoGroup(err) && try == 0 {
			if gerr := c.ensureGroup(ctx, stream); gerr != nil {
				return nil, nil // stream vanished; the API's cleanup will drop it from the set
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, s := range res {
			for _, m := range s.Messages {
				return toMessage(stream, m, 1, false), nil
			}
		}
		return nil, nil
	}
	return nil, nil
}

func toMessage(stream string, m redis.XMessage, deliveries int64, reclaimed bool) *Message {
	var data []byte
	if v, ok := m.Values["data"]; ok {
		switch t := v.(type) {
		case string:
			data = []byte(t)
		case []byte:
			data = t
		default:
			data = []byte(fmt.Sprint(t))
		}
	}
	return &Message{Stream: stream, ID: m.ID, Lane: LaneOf(stream), Data: data, Deliveries: deliveries, Reclaimed: reclaimed}
}

// Next performs one priority pass (§7.1). Returns nil, nil when every stream is empty.
func (c *Consumer) Next(ctx context.Context, consumer string) (*Message, error) {
	for _, lane := range c.opts.Lanes {
		list, err := c.laneStreams(ctx, lane)
		if err != nil {
			return nil, err
		}
		for _, s := range c.rotation(lane, list) {
			m, err := c.readOne(ctx, s, consumer)
			if err != nil {
				return nil, err
			}
			if m != nil {
				return m, nil
			}
		}
	}
	return nil, nil
}

// ReclaimDue reports whether this caller should run a reclaim pass now. Only one caller
// wins per reclaim_every; after a pass that found work, the next caller may try again.
func (c *Consumer) ReclaimDue() bool {
	if c.reclaimAgain.CompareAndSwap(true, false) {
		return true
	}
	last := c.lastReclaim.Load()
	now := c.now().UnixNano()
	if now-last < int64(c.opts.ReclaimEvery) {
		return false
	}
	return c.lastReclaim.CompareAndSwap(last, now)
}

// Reclaim claims at most one message idle longer than reclaim_min_idle, in lane priority
// order, into consumer. Deliveries comes from XPENDING (after the claim).
func (c *Consumer) Reclaim(ctx context.Context, consumer string) (*Message, error) {
	for _, lane := range c.opts.Lanes {
		list, err := c.laneStreams(ctx, lane)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			start := "0-0"
			for {
				msgs, next, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
					Stream: s, Group: c.opts.Group, MinIdle: c.opts.ReclaimMinIdle,
					Start: start, Count: 1, Consumer: consumer,
				}).Result()
				if isNoGroup(err) || errors.Is(err, redis.Nil) {
					break
				}
				if err != nil {
					return nil, err
				}
				for _, m := range msgs {
					if len(m.Values) == 0 { // deleted entry (Redis 6.2 semantics)
						_ = c.rdb.XAck(ctx, s, c.opts.Group, m.ID).Err()
						continue
					}
					n := c.deliveries(ctx, s, m.ID)
					c.reclaimAgain.Store(true)
					return toMessage(s, m, n, true), nil
				}
				if next == "0-0" || next == "" {
					break
				}
				start = next
			}
		}
	}
	return nil, nil
}

func (c *Consumer) deliveries(ctx context.Context, stream, id string) int64 {
	p, err := c.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream, Group: c.opts.Group, Start: id, End: id, Count: 1,
	}).Result()
	if err != nil || len(p) == 0 {
		return 2
	}
	return p[0].RetryCount
}

// Heartbeat resets the idle time of an in-flight message (XCLAIM … 0 <id> JUSTID).
func (c *Consumer) Heartbeat(ctx context.Context, stream, id, consumer string) error {
	return c.rdb.XClaimJustID(ctx, &redis.XClaimArgs{
		Stream: stream, Group: c.opts.Group, Consumer: consumer, MinIdle: 0, Messages: []string{id},
	}).Err()
}

// Ack = XACK then XDEL (§7.2).
func (c *Consumer) Ack(ctx context.Context, stream, id string) error {
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.XAck(ctx, stream, c.opts.Group, id)
		p.XDel(ctx, stream, id)
		return nil
	})
	return err
}

// SpoolAcker adapts the consumer to report.Acker for acks deferred by the spool.
type SpoolAcker struct {
	C        *Consumer
	Consumer string
}

func (a SpoolAcker) Ack(ctx context.Context, stream, id string) error {
	return a.C.Ack(ctx, stream, id)
}
func (a SpoolAcker) Touch(ctx context.Context, stream, id string) error {
	return a.C.Heartbeat(ctx, stream, id, a.Consumer)
}
