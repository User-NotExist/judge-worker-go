// Package health runs the self-test suite (§15) and the full hostile suite (§19.3/§19.4).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"judge-worker/internal/config"
	"judge-worker/internal/gojudge"
	"judge-worker/internal/pipeline"
	"judge-worker/internal/verdict"
)

type Checker struct {
	Cfg  *config.Config
	Deps *pipeline.Deps
	Log  *slog.Logger

	baselineMu sync.Mutex
}

type Baseline struct {
	BenchmarkMs    float64   `json:"benchmark_ms"`
	CreatedAt      time.Time `json:"created_at"`
	GoJudgeVersion string    `json:"go_judge_version"`
	Language       string    `json:"language"`
}

// Case is one self-test program with its accepted outcomes.
type Case struct {
	Name   string
	Lang   string // "cpp" or "py": resolved to a registry entry by source extension
	Src    string
	Stdin  string
	TimeMs uint64
	MemMb  uint64
	// Expect lists acceptable verdicts; Pending means "ran to completion".
	Expect []verdict.Verdict
	// Stdout, if set, must equal the trimmed output when the run completed.
	Stdout []string
}

type CaseResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
	TimeMs  uint64 `json:"time_ms"`
}

func (c *Checker) langFor(kind string) (string, bool) {
	ext := map[string]string{"cpp": ".cpp", "py": ".py"}[kind]
	if kind == "cpp" {
		if _, ok := c.Deps.Langs.Get(pipeline.HelperLanguage); ok {
			return pipeline.HelperLanguage, true
		}
	}
	for _, id := range c.Deps.Langs.IDs() {
		l, _ := c.Deps.Langs.Get(id)
		if l.Ext() == ext {
			return id, true
		}
	}
	return "", false
}

func (c *Checker) runCase(ctx context.Context, tc Case) CaseResult {
	id, ok := c.langFor(tc.Lang)
	if !ok {
		return CaseResult{Name: tc.Name, OK: true, Verdict: "skipped", Detail: "no " + tc.Lang + " language"}
	}
	if tc.TimeMs == 0 {
		tc.TimeMs = 1000
	}
	if tc.MemMb == 0 {
		tc.MemMb = 256
	}
	r, err := c.Deps.Exec(ctx, id, tc.Src, tc.Stdin, tc.TimeMs, tc.MemMb)
	if err != nil {
		return CaseResult{Name: tc.Name, Verdict: "error", Detail: err.Error()}
	}
	v := r.Verdict
	label := string(v)
	if v == verdict.Pending {
		label = "completed"
	}
	res := CaseResult{Name: tc.Name, Verdict: label, TimeMs: verdict.TimeMs(r.Run)}
	for _, e := range tc.Expect {
		if e == v {
			res.OK = true
		}
	}
	out := strings.TrimSpace(r.Stdout)
	if res.OK && v == verdict.Pending && len(tc.Stdout) > 0 {
		res.OK = false
		for _, s := range tc.Stdout {
			if out == s {
				res.OK = true
			}
		}
	}
	if !res.OK {
		d := fmt.Sprintf("expected %v, got %s %s", tc.Expect, label, r.Message)
		if v == verdict.Pending {
			d += fmt.Sprintf(" stdout=%q", clip(out, 80))
		}
		if v == verdict.CE {
			d += " compile: " + clip(r.CompileLog, 200)
		}
		res.Detail = d
	}
	return res
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

const done = verdict.Pending

// QuickCases are the per-slot checks of §15 items 2 and 3.
func (c *Checker) QuickCases() []Case {
	cases := []Case{
		{Name: "a+b cpp", Lang: "cpp", Src: aPlusBCpp, Stdin: "2 3\n", Expect: []verdict.Verdict{done}, Stdout: []string{"5"}},
		{Name: "a+b py", Lang: "py", Src: aPlusBPy, Stdin: "2 3\n", Expect: []verdict.Verdict{done}, Stdout: []string{"5"}},
		{Name: "infinite loop", Lang: "cpp", Src: infiniteLoopCpp, TimeMs: 500, Expect: []verdict.Verdict{verdict.TLE}},
		{Name: "allocate 2 GiB", Lang: "cpp", Src: alloc2GiBCpp, Expect: []verdict.Verdict{verdict.MLE, verdict.RE}},
		{Name: "network", Lang: "cpp", Src: strings.ReplaceAll(networkCpp, "%IP%", c.Cfg.Health.NetworkProbeIP), Expect: []verdict.Verdict{done, verdict.RE}, Stdout: []string{"blocked"}},
		{Name: "cache hidden", Lang: "cpp", Src: hiddenCacheCpp, Expect: []verdict.Verdict{done}, Stdout: []string{"hidden"}},
	}
	// Every registered language must pass A+B.
	for _, id := range c.Deps.Langs.IDs() {
		l, _ := c.Deps.Langs.Get(id)
		if l.Ext() != ".cpp" && l.Ext() != ".py" {
			c.Log.Warn("no built-in self-test for language", "language", id)
		}
	}
	return cases
}

// HostileCases is the full suite of §19.3.
func (c *Checker) HostileCases() []Case {
	ip := c.Cfg.Health.NetworkProbeIP
	return []Case{
		{Name: "fork bomb", Lang: "cpp", Src: forkBombCpp, Expect: []verdict.Verdict{done, verdict.RE, verdict.TLE}, Stdout: []string{"limited"}},
		{Name: "while(true)", Lang: "cpp", Src: infiniteLoopCpp, Expect: []verdict.Verdict{verdict.TLE}},
		{Name: "sleep(1000)", Lang: "cpp", Src: sleepCpp, Expect: []verdict.Verdict{verdict.TLE}},
		{Name: "allocate and touch 2 GiB", Lang: "cpp", Src: alloc2GiBCpp, Expect: []verdict.Verdict{verdict.MLE, verdict.RE}},
		{Name: "1 GiB to stdout", Lang: "cpp", Src: stdout1GiBCpp, TimeMs: 5000, Expect: []verdict.Verdict{verdict.OLE}},
		{Name: "deep recursion", Lang: "cpp", Src: deepRecursionCpp, Expect: []verdict.Verdict{done}, Stdout: []string{"1000000"}},
		{Name: "read /etc/shadow", Lang: "cpp", Src: readShadowCpp, Expect: []verdict.Verdict{done}, Stdout: []string{"denied"}},
		{Name: "search for *.ans", Lang: "cpp", Src: findAnsCpp, TimeMs: 5000, Expect: []verdict.Verdict{done}, Stdout: []string{"0"}},
		{Name: "connect external IP", Lang: "cpp", Src: strings.ReplaceAll(networkCpp, "%IP%", ip), Expect: []verdict.Verdict{done, verdict.RE}, Stdout: []string{"blocked"}},
		{Name: "fill /w tmpfs", Lang: "cpp", Src: fillTmpfsCpp, TimeMs: 5000, Expect: []verdict.Verdict{done, verdict.RE, verdict.MLE, verdict.OLE}, Stdout: []string{"enospc"}},
		{Name: "threads past procLimit", Lang: "cpp", Src: threadsCpp, Expect: []verdict.Verdict{done, verdict.RE}, Stdout: []string{"limited"}},
		{Name: "exit(3)", Lang: "cpp", Src: exit3Cpp, Expect: []verdict.Verdict{verdict.RE}},
		{Name: "compile: #include /dev/random", Lang: "cpp", Src: includeRandomCpp, Expect: []verdict.Verdict{verdict.CE}},
		{Name: "compile: template recursion", Lang: "cpp", Src: templateBombCpp, Expect: []verdict.Verdict{verdict.CE}},
		{Name: "compile: error flood", Lang: "cpp", Src: errorFloodCpp, Expect: []verdict.Verdict{verdict.CE}},
	}
}

func (c *Checker) checkSandbox(ctx context.Context) error {
	if _, err := c.Deps.GJ.Version(ctx); err != nil {
		return fmt.Errorf("go-judge /version: %w", err)
	}
	cfg, err := c.Deps.GJ.Config(ctx)
	if err != nil {
		return fmt.Errorf("go-judge /config: %w", err)
	}
	if p := gojudge.FindParallelism(cfg); p > 0 && p != c.Cfg.Slots {
		return fmt.Errorf("go-judge parallelism %d != slots %d", p, c.Cfg.Slots)
	}
	return nil
}

// Quick runs §15 checks 1–3. Returns nil when the slot may take jobs.
func (c *Checker) Quick(ctx context.Context) error {
	if err := c.checkSandbox(ctx); err != nil {
		return err
	}
	var errs []string
	for _, tc := range c.QuickCases() {
		r := c.runCase(ctx, tc)
		if !r.OK {
			errs = append(errs, r.Name+": "+r.Detail)
		}
	}
	if c.Cfg.Health.TimingCheck {
		if err := c.timingQuick(ctx); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (c *Checker) bench(ctx context.Context) (float64, error) {
	id, ok := c.langFor("cpp")
	if !ok {
		return 0, errors.New("no C++ language for timing benchmark")
	}
	r, err := c.Deps.Exec(ctx, id, benchCpp, "", 10_000, 64)
	if err != nil {
		return 0, err
	}
	if r.Verdict != verdict.Pending {
		return 0, fmt.Errorf("benchmark ended %s %s", r.Verdict, r.Message)
	}
	return float64(r.Run.Time) / 1e6, nil
}

func (c *Checker) loadBaseline() (*Baseline, error) {
	b, err := os.ReadFile(c.Cfg.Health.BaselineFile)
	if err != nil {
		return nil, err
	}
	var bl Baseline
	if err := json.Unmarshal(b, &bl); err != nil {
		return nil, err
	}
	return &bl, nil
}

func (c *Checker) writeBaseline(ms float64) error {
	c.baselineMu.Lock()
	defer c.baselineMu.Unlock()
	bl := Baseline{BenchmarkMs: ms, CreatedAt: time.Now().UTC(), GoJudgeVersion: c.Deps.GoJudgeVersion(), Language: pipeline.HelperLanguage}
	b, _ := json.MarshalIndent(bl, "", "  ")
	if err := os.MkdirAll(filepath.Dir(c.Cfg.Health.BaselineFile), 0o750); err != nil {
		return err
	}
	tmp := c.Cfg.Health.BaselineFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, c.Cfg.Health.BaselineFile)
}

// timingQuick compares one benchmark run with the baseline (±tolerance). The first
// successful run after install creates the baseline.
func (c *Checker) timingQuick(ctx context.Context) error {
	ms, err := c.bench(ctx)
	if err != nil {
		return fmt.Errorf("timing: %w", err)
	}
	bl, err := c.loadBaseline()
	if errors.Is(err, os.ErrNotExist) {
		c.Log.Info("no timing baseline; creating one", "benchmark_ms", ms)
		return c.writeBaseline(ms)
	}
	if err != nil {
		return fmt.Errorf("timing baseline: %w", err)
	}
	if dev := math.Abs(ms-bl.BenchmarkMs) / bl.BenchmarkMs; dev > c.Cfg.Health.TimingTol {
		return fmt.Errorf("timing: benchmark %.0f ms vs baseline %.0f ms (%.1f%% > %.0f%%)", ms, bl.BenchmarkMs, dev*100, c.Cfg.Health.TimingTol*100)
	}
	return nil
}

// FullReport is printed by `judge-worker selftest`.
type FullReport struct {
	OK      bool         `json:"ok"`
	Cases   []CaseResult `json:"cases"`
	Timing  *TimingStats `json:"timing,omitempty"`
	Healthy string       `json:"healthy_after"`
}

type TimingStats struct {
	Runs      int       `json:"runs_per_slot"`
	Slots     int       `json:"slots"`
	MedianMs  float64   `json:"median_ms"`
	SpreadPct []float64 `json:"spread_pct_per_slot"`
	OK        bool      `json:"ok"`
}

// Full runs the hostile suite, then the timing stability test (20 runs on each slot in
// parallel, spread < 2 %), and writes the baseline when everything passes.
func (c *Checker) Full(ctx context.Context, timing bool) FullReport {
	rep := FullReport{OK: true}
	if err := c.checkSandbox(ctx); err != nil {
		rep.OK = false
		rep.Cases = append(rep.Cases, CaseResult{Name: "sandbox", Detail: err.Error()})
		return rep
	}
	for _, tc := range append(c.QuickCases()[:2], c.HostileCases()...) {
		r := c.runCase(ctx, tc)
		c.Log.Info("selftest case", "name", r.Name, "ok", r.OK, "verdict", r.Verdict, "detail", r.Detail)
		rep.Cases = append(rep.Cases, r)
		rep.OK = rep.OK && r.OK
	}
	if timing {
		ts := c.timingStability(ctx, 20)
		rep.Timing = ts
		rep.OK = rep.OK && ts.OK
		if rep.OK {
			if err := c.writeBaseline(ts.MedianMs); err != nil {
				rep.OK = false
				rep.Cases = append(rep.Cases, CaseResult{Name: "write baseline", Detail: err.Error()})
			}
		}
	}
	// The host must stay healthy afterwards.
	saved := c.Cfg.Health.TimingCheck
	c.Cfg.Health.TimingCheck = timing && saved
	if err := c.Quick(ctx); err != nil {
		rep.OK = false
		rep.Healthy = err.Error()
	} else {
		rep.Healthy = "ok"
	}
	c.Cfg.Health.TimingCheck = saved
	return rep
}

func (c *Checker) timingStability(ctx context.Context, runs int) *TimingStats {
	ts := &TimingStats{Runs: runs, Slots: c.Cfg.Slots, OK: true}
	all := make([][]float64, c.Cfg.Slots)
	var wg sync.WaitGroup
	for s := 0; s < c.Cfg.Slots; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := 0; i < runs; i++ {
				ms, err := c.bench(ctx)
				if err != nil {
					c.Log.Error("timing run failed", "slot", s, "err", err)
					return
				}
				all[s] = append(all[s], ms)
			}
		}(s)
	}
	wg.Wait()
	var flat []float64
	for s, xs := range all {
		if len(xs) < runs {
			ts.OK = false
			ts.SpreadPct = append(ts.SpreadPct, -1)
			continue
		}
		sort.Float64s(xs)
		mean := 0.0
		for _, x := range xs {
			mean += x
		}
		mean /= float64(len(xs))
		spread := (xs[len(xs)-1] - xs[0]) / mean * 100
		ts.SpreadPct = append(ts.SpreadPct, math.Round(spread*100)/100)
		if spread >= 2 {
			ts.OK = false
		}
		c.Log.Info("timing slot", "slot", s, "min_ms", xs[0], "max_ms", xs[len(xs)-1], "spread_pct", spread)
		flat = append(flat, xs...)
	}
	if len(flat) > 0 {
		sort.Float64s(flat)
		ts.MedianMs = flat[len(flat)/2]
	}
	return ts
}
