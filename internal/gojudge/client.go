package gojudge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnavailable means go-judge could not be reached (transport error or 5xx) even after
// the retry. The pipeline turns this into "sandbox_unavailable" (§14).
var ErrUnavailable = errors.New("go-judge unavailable")

type Client interface {
	Run(ctx context.Context, req Request) ([]Result, error)
	GetFile(ctx context.Context, id string, w io.Writer, max int64) (int64, error)
	UploadFile(ctx context.Context, name string, r io.Reader) (string, error)
	DeleteFile(ctx context.Context, id string) error
	Version(ctx context.Context) (string, error)
	Config(ctx context.Context) (map[string]any, error)
}

// Observer receives per-request outcomes for metrics. Kind is "" on success.
type Observer func(status string, errKind string)

type HTTPClient struct {
	base      string
	token     string
	extra     time.Duration
	http      *http.Client
	observe   Observer
	retryWait time.Duration
}

func New(baseURL, token string, timeoutExtra time.Duration, obs Observer) *HTTPClient {
	if obs == nil {
		obs = func(string, string) {}
	}
	return &HTTPClient{
		base:  strings.TrimSuffix(baseURL, "/"),
		token: token,
		extra: timeoutExtra,
		// Per-call timeouts come from contexts; no global timeout.
		http:      &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second}},
		observe:   obs,
		retryWait: time.Second,
	}
}

type httpStatusErr struct {
	code int
	body string
}

func (e *httpStatusErr) Error() string { return fmt.Sprintf("go-judge HTTP %d: %s", e.code, e.body) }

func (c *HTTPClient) do(ctx context.Context, method, path string, body func() (io.Reader, string), timeout time.Duration, handle func(*http.Response) error) error {
	var lastErr error
	for try := 0; try < 2; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.retryWait):
			}
		}
		err := c.once(ctx, method, path, body, timeout, handle)
		if err == nil {
			return nil
		}
		lastErr = err
		var se *httpStatusErr
		if errors.As(err, &se) && se.code < 500 {
			return err // 4xx: not retryable
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.observe("", "transport")
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
}

func (c *HTTPClient) once(ctx context.Context, method, path string, body func() (io.Reader, string), timeout time.Duration, handle func(*http.Response) error) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var rd io.Reader
	ctype := ""
	if body != nil {
		rd, ctype = body()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpStatusErr{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	return handle(resp)
}

func decodeJSON(dst any) func(*http.Response) error {
	return func(r *http.Response) error { return json.NewDecoder(r.Body).Decode(dst) }
}

// Run executes one request. HTTP timeout = max clockLimit + request_timeout_extra (§8).
func (c *HTTPClient) Run(ctx context.Context, req Request) ([]Result, error) {
	var maxClock uint64
	for _, cmd := range req.Cmd {
		if cmd.ClockLimit > maxClock {
			maxClock = cmd.ClockLimit
		}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var res []Result
	err = c.do(ctx, http.MethodPost, "/run", func() (io.Reader, string) {
		return bytes.NewReader(payload), "application/json"
	}, time.Duration(maxClock)+c.extra, decodeJSON(&res))
	if err != nil {
		return nil, err
	}
	if len(res) != len(req.Cmd) {
		return nil, fmt.Errorf("go-judge returned %d results for %d commands", len(res), len(req.Cmd))
	}
	for _, r := range res {
		c.observe(r.Status, "")
	}
	return res, nil
}

// GetFile streams a file-store entry into w, failing if it exceeds max bytes (max<=0: no cap).
func (c *HTTPClient) GetFile(ctx context.Context, id string, w io.Writer, max int64) (int64, error) {
	var n int64
	err := c.do(ctx, http.MethodGet, "/file/"+url.PathEscape(id), nil, 2*time.Minute, func(r *http.Response) error {
		src := io.Reader(r.Body)
		if max > 0 {
			src = io.LimitReader(r.Body, max+1)
		}
		var err error
		n, err = io.Copy(w, src)
		if err == nil && max > 0 && n > max {
			return fmt.Errorf("file %s larger than %d bytes", id, max)
		}
		return err
	})
	return n, err
}

// UploadFile stores r in the go-judge file store. r is buffered so the retry can resend it.
func (c *HTTPClient) UploadFile(ctx context.Context, name string, r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	var id string
	err = c.do(ctx, http.MethodPost, "/file", func() (io.Reader, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write(data)
		_ = mw.Close()
		return &buf, mw.FormDataContentType()
	}, 2*time.Minute, decodeJSON(&id))
	return id, err
}

func (c *HTTPClient) DeleteFile(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/file/"+url.PathEscape(id), nil, 10*time.Second, func(*http.Response) error { return nil })
}

func (c *HTTPClient) Version(ctx context.Context) (string, error) {
	var v map[string]any
	if err := c.do(ctx, http.MethodGet, "/version", nil, 5*time.Second, decodeJSON(&v)); err != nil {
		return "", err
	}
	for _, k := range []string{"buildVersion", "version"} {
		if s, ok := v[k].(string); ok && s != "" {
			return s, nil
		}
	}
	return "unknown", nil
}

func (c *HTTPClient) Config(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	err := c.do(ctx, http.MethodGet, "/config", nil, 5*time.Second, decodeJSON(&v))
	return v, err
}

// FindParallelism searches the /config response for a "parallelism" key (any depth).
// Returns 0 if the pinned go-judge version does not expose it.
func FindParallelism(cfg map[string]any) int {
	var walk func(v any) int
	walk = func(v any) int {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if strings.EqualFold(k, "parallelism") {
					if f, ok := x.(float64); ok {
						return int(f)
					}
				}
			}
			for _, x := range t {
				if n := walk(x); n > 0 {
					return n
				}
			}
		}
		return 0
	}
	return walk(cfg)
}
