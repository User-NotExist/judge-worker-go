package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// Client posts events. Post returns an Outcome per §12 or a retryable error.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

type retryable struct{ err error }

func (r retryable) Error() string { return r.err.Error() }

func IsRetryable(err error) bool {
	var r retryable
	return errors.As(err, &r)
}

// post sends one event (object) or many (array).
func (c *Client) post(ctx context.Context, body any) (Outcome, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return Dropped, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(b))
	if err != nil {
		return Dropped, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, retryable{err}
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode/100 == 2:
		return Accepted, nil
	case resp.StatusCode == http.StatusConflict:
		return Superseded, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return 0, retryable{fmt.Errorf("events API HTTP %d", resp.StatusCode)}
	default:
		return Dropped, fmt.Errorf("events API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// Backoff is exponential 0.5 s → 30 s with full jitter.
func Backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << min(attempt, 6)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// PostWithRetry tries up to `tries` times with backoff; a retryable error after the last
// try is returned so the caller can spool.
func (c *Client) PostWithRetry(ctx context.Context, body any, tries int) (Outcome, error) {
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, retryable{ctx.Err()}
			case <-time.After(Backoff(i - 1)):
			}
		}
		var o Outcome
		o, err = c.post(ctx, body)
		if err == nil || !IsRetryable(err) {
			return o, err
		}
	}
	return 0, err
}
