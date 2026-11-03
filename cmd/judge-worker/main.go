// Command judge-worker pulls judge jobs from Redis, runs them in go-judge and reports
// events to the API. Subcommands: run (default), healthcheck, selftest, warm, version.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"judge-worker/internal/config"
	"judge-worker/internal/gojudge"
	"judge-worker/internal/health"
	"judge-worker/internal/lang"
	"judge-worker/internal/metrics"
	"judge-worker/internal/pipeline"
	"judge-worker/internal/queue"
	"judge-worker/internal/report"
	"judge-worker/internal/testdata"
)

var version = "dev"

const defaultConfig = "/etc/judge-worker/config.yaml"

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig, "config file")
	noTiming := fs.Bool("skip-timing", false, "selftest: skip the timing stability test (test profile only)")
	pkgSHA := fs.String("package-sha", "", "warm: package sha256")
	pkgKey := fs.String("package-key", "", "warm: package object key")
	_ = fs.Parse(args)

	var code int
	switch cmd {
	case "run":
		code = run(*cfgPath)
	case "healthcheck":
		code = healthcheck(*cfgPath)
	case "selftest":
		code = selftest(*cfgPath, !*noTiming)
	case "warm":
		code = warm(*cfgPath, *pkgSHA, *pkgKey)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (run | healthcheck | selftest | warm | version)\n", cmd)
		code = 2
	}
	os.Exit(code)
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

func httpClient(caFile string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file contains no certificates")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr}, nil
}

// core holds what every subcommand that talks to go-judge needs.
type core struct {
	cfg  *config.Config
	log  *slog.Logger
	m    *metrics.M
	deps *pipeline.Deps
	http *http.Client
}

func setup(cfgPath string) (*core, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	log := newLogger(cfg.LogLevel).With("worker_id", cfg.WorkerID)
	slog.SetDefault(log)
	langs, err := lang.Load(cfg.LanguagesFile)
	if err != nil {
		return nil, err
	}
	if _, ok := langs.Get(pipeline.HelperLanguage); !ok {
		return nil, fmt.Errorf("languages: %s is required for checkers and managers", pipeline.HelperLanguage)
	}
	hc, err := httpClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	m := metrics.New()
	gj := gojudge.New(cfg.GoJudge.URL, cfg.GoJudgeToken, cfg.GoJudge.RequestTimeoutExtra.D(), func(status, errKind string) {
		if errKind != "" {
			m.GoJudgeErrors.WithLabelValues(errKind).Inc()
			return
		}
		m.GoJudgeRequests.WithLabelValues(status).Inc()
	})
	fetcher, err := testdata.NewS3Fetcher(cfg.Storage.Endpoint, cfg.Storage.Bucket, cfg.Storage.Region, cfg.S3AccessKey, cfg.S3SecretKey, hc.Transport)
	if err != nil {
		return nil, err
	}
	deps := &pipeline.Deps{Cfg: cfg, GJ: gj, Langs: langs, HTTP: hc, Metrics: m}
	deps.Cache = &testdata.Cache{
		Dir: cfg.Cache.Dir, MaxBytes: cfg.Cache.MaxBytes.U(), Fetch: fetcher, Compile: deps.HelperCompiler(),
		Log: log, OnSize: func(n uint64) { m.CacheBytes.Set(float64(n)) },
	}
	return &core{cfg: cfg, log: log, m: m, deps: deps, http: hc}, nil
}

func run(cfgPath string) int {
	c, err := setup(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge-worker:", err)
		return 1
	}
	cfg, log := c.cfg, c.log
	log.Info("judge-worker starting", "version", version, "slots", cfg.Slots, "lanes", cfg.Redis.Lanes)

	// Signals: first SIGTERM stops pulling and drains; second exits immediately.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	pullCtx, stopPull := context.WithCancel(context.Background())
	defer stopPull()
	jobCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()
	bgCtx, stopBG := context.WithCancel(context.Background())
	defer stopBG()

	ver, err := startupChecks(pullCtx, cfg, c.deps.GJ, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 1
	}
	c.deps.SetGoJudgeVersion(ver)
	for _, d := range []string{cfg.ScratchDir, cfg.API.SpoolDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			log.Error("startup failed", "err", err)
			return 1
		}
	}
	if err := c.deps.Cache.Init(); err != nil {
		log.Error("cache init failed", "err", err)
		return 1
	}
	c.deps.ProbeToolchains(pullCtx)

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.RedisPassword, PoolSize: cfg.Slots*2 + 4})
	defer rdb.Close()
	if err := rdb.Ping(pullCtx).Err(); err != nil {
		log.Error("redis unreachable", "err", err)
		return 1
	}
	consumer := queue.New(rdb, queue.Options{
		Group: cfg.Redis.Group, Lanes: cfg.Redis.Lanes,
		ReclaimMinIdle: cfg.Redis.ReclaimMinIdle.D(), ReclaimEvery: cfg.Redis.ReclaimEvery.D(),
	})

	client := &report.Client{URL: cfg.API.EventsURL, Token: cfg.APIToken, HTTP: c.http}
	spool := &report.Spool{Dir: cfg.API.SpoolDir}
	if err := spool.Init(); err != nil {
		log.Error("spool init failed", "err", err)
		return 1
	}
	replayer := &report.Replayer{
		Spool: spool, Client: client, Log: log, Heartbeat: cfg.Redis.Heartbeat.D(),
		Acker:   queue.SpoolAcker{C: consumer, Consumer: cfg.WorkerID + "/spool"},
		OnGauge: func(n int) { c.m.EventsSpooled.Set(float64(n)) }, OnDropped: c.m.EventsDropped.Inc,
	}
	var bgWG sync.WaitGroup
	bgWG.Add(1)
	go func() { defer bgWG.Done(); replayer.Run(bgCtx) }()

	hc := &health.Checker{Cfg: cfg, Deps: c.deps, Log: log}
	w := &Worker{
		cfg: cfg, log: log, deps: c.deps, consumer: consumer, client: client, spool: spool,
		replayer: replayer, health: hc, m: c.m, apiHTTP: c.http,
		slotHealthy: make([]bool, cfg.Slots), healthKick: make([]chan struct{}, cfg.Slots),
	}
	for i := range w.healthKick {
		w.healthKick[i] = make(chan struct{}, 1)
	}

	// Metrics + /healthz.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(c.m.Reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		if w.Healthy() {
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte("ok\n"))
			return
		}
		rw.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rw.Write([]byte("no healthy slot\n"))
	})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server failed", "err", err)
		}
	}()

	// Startup self-test (§15): one run, applied to every slot.
	hctx, hcancel := context.WithTimeout(pullCtx, 5*time.Minute)
	herr := hc.Quick(hctx)
	hcancel()
	if herr != nil {
		log.Error("startup health check failed; all slots quarantined", "err", herr)
		c.m.Alert("slot_unhealthy")
	} else {
		log.Info("startup health check passed")
	}

	var wg sync.WaitGroup
	for s := 0; s < cfg.Slots; s++ {
		wg.Add(1)
		go func(s int) { defer wg.Done(); w.runSlot(pullCtx, jobCtx, s, herr == nil) }(s)
	}

	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()

	terms := 0
loop:
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGUSR1 {
				log.Info("SIGUSR1: running health checks")
				w.KickHealth()
				continue
			}
			terms++
			if terms == 1 {
				log.Info("shutdown: stop pulling, waiting for running jobs (max 5 min)")
				stopPull()
				go func() {
					select {
					case <-drained:
					case <-time.After(5 * time.Minute):
						log.Warn("shutdown: jobs still running after 5 min; cancelling")
						cancelJobs()
					}
				}()
			} else {
				log.Warn("second signal: exiting immediately; unfinished jobs will be reclaimed")
				os.Exit(1)
			}
		case <-drained:
			break loop
		}
	}

	// Flush the spool once more, then stop background work.
	fctx, fcancel := context.WithTimeout(context.Background(), 20*time.Second)
	replayer.Pass(fctx)
	fcancel()
	stopBG()
	bgWG.Wait()
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(sctx)
	scancel()
	log.Info("judge-worker stopped")
	return 0
}

// healthcheck asks the running worker's /healthz (distroless: no curl in the image).
func healthcheck(cfgPath string) int {
	addr := "127.0.0.1:9100"
	if cfg, err := config.Load(cfgPath); err == nil {
		if _, port, err := net.SplitHostPort(cfg.MetricsAddr); err == nil {
			addr = "127.0.0.1:" + port
		}
	}
	cl := &http.Client{Timeout: 4 * time.Second}
	resp, err := cl.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}

func selftest(cfgPath string, timing bool) int {
	c, err := setup(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge-worker:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ver, err := c.deps.GJ.Version(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "go-judge:", err)
		return 1
	}
	c.deps.SetGoJudgeVersion(ver)
	hc := &health.Checker{Cfg: c.cfg, Deps: c.deps, Log: c.log}
	rep := hc.Full(ctx, timing)
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Fprintln(os.Stderr, string(b))
	if !rep.OK {
		return 1
	}
	return 0
}

// warm pre-loads a package into the cache (exam-day checklist, §18.6).
func warm(cfgPath, sha, key string) int {
	if sha == "" || key == "" {
		fmt.Fprintln(os.Stderr, "usage: judge-worker warm -package-sha <sha256> -package-key <key>")
		return 2
	}
	c, err := setup(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge-worker:", err)
		return 1
	}
	if err := c.deps.Cache.Init(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	p, err := c.deps.Cache.Ensure(ctx, sha, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warm failed:", err)
		return 1
	}
	p.Release()
	fmt.Printf("cached %s (%d testcases)\n", sha, len(p.Problem.Testcases))
	return 0
}
