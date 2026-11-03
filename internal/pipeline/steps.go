package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"judge-worker/internal/checker"
	"judge-worker/internal/gojudge"
	"judge-worker/internal/job"
	"judge-worker/internal/lang"
	"judge-worker/internal/report"
	"judge-worker/internal/testdata"
	"judge-worker/internal/verdict"
)

const maxSourceBytes = 16 << 20

// CompileOutcome of a submission / reference compile.
type CompileOutcome struct {
	OK        bool
	FileID    string
	TimeMs    uint64
	Log       string
	Truncated bool
}

func (d *Deps) compileCmd(l *lang.Language, names []string, files map[string][]byte, extraFlags ...string) gojudge.Cmd {
	lim := d.Cfg.Limits.Compile
	copyIn := make(map[string]*gojudge.File, len(files))
	for n, b := range files {
		copyIn[n] = &gojudge.File{Content: gojudge.Str(string(b))}
	}
	args := l.CompileArgs(names)
	if len(extraFlags) > 0 {
		args = l.CompileArgsExtra(names, extraFlags...)
	}
	return gojudge.Cmd{
		Args:     args,
		Env:      l.Env,
		Files:    []*gojudge.File{gojudge.EmptyStdin(), gojudge.Collector("stdout", lim.Output.U()), gojudge.Collector("stderr", lim.Output.U())},
		CPULimit: gojudge.Dur(lim.CPU.D()), ClockLimit: gojudge.Dur(lim.Wall.D()),
		MemoryLimit: lim.Memory.U(), ProcLimit: lim.Procs,
		CopyIn:        copyIn,
		CopyOutCached: []string{l.Compile.Artifact},
	}
}

func compileLog(r gojudge.Result, max int) (string, bool) {
	log := r.Files["stdout"] + r.Files["stderr"]
	if r.Overflowed("stdout") || r.Overflowed("stderr") {
		r.Status = gojudge.StatusOutputLimit
	}
	switch r.Status {
	case gojudge.StatusAccepted, gojudge.StatusNonzeroExit:
	case gojudge.StatusTimeLimit:
		log += "\n[compilation time limit exceeded]"
	case gojudge.StatusMemoryLimit:
		log += "\n[compilation memory limit exceeded]"
	case gojudge.StatusOutputLimit:
		log += "\n[compiler output limit exceeded]"
		return truncateLog(log, max, true)
	default:
		log += fmt.Sprintf("\n[compiler %s]", r.Status)
	}
	return truncateLog(log, max, false)
}

func truncateLog(s string, max int, forced bool) (string, bool) {
	t, cut := checker.Truncate(s, max)
	return t, cut || forced
}

// compileSubmission compiles source files (in job order) — §10.1.
func (jc *jobCtx) compileSubmission(ctx context.Context, l *lang.Language, refs []job.FileRef) (CompileOutcome, error) {
	names := make([]string, len(refs))
	for i, f := range refs {
		names[i] = f.Name
	}
	res, err := jc.runRetry(ctx, gojudge.Request{Cmd: []gojudge.Cmd{jc.d.compileCmd(l, names, jc.sources)}})
	if err != nil {
		return CompileOutcome{}, err
	}
	r := res[0]
	out := CompileOutcome{TimeMs: verdict.TimeMs(r)}
	out.Log, out.Truncated = compileLog(r, int(jc.d.Cfg.Limits.Compile.Output.U()))
	if id, ok := r.FileIDs[l.Compile.Artifact]; ok && r.Status == gojudge.StatusAccepted {
		out.OK, out.FileID = true, id
	}
	return out, nil
}

// runCopyIn returns the files a contestant run needs: the artifact, plus the other
// sources when the artifact is itself a source (interpreted languages with harness files).
func (jc *jobCtx) runCopyIn() map[string]*gojudge.File {
	in := map[string]*gojudge.File{jc.lang.Compile.Artifact: {FileID: jc.binary}}
	if _, isSource := jc.sources[jc.lang.Compile.Artifact]; isSource {
		for n, b := range jc.sources {
			if n != jc.lang.Compile.Artifact {
				in[n] = &gojudge.File{Content: gojudge.Str(string(b))}
			}
		}
	}
	return in
}

// caseRun is the contestant-run part of a testcase.
type caseRun struct {
	res      gojudge.Result
	mapping  verdict.Mapping
	stdoutID string
}

// runBatchCase runs one testcase (§10.2).
func (jc *jobCtx) runBatchCase(ctx context.Context, tc string) (caseRun, error) {
	tl := jc.job.Limits.TimeMs
	mem := jc.job.Limits.MemoryMb
	cpu := gojudge.Ms(tl)
	cmd := gojudge.Cmd{
		Args: jc.lang.Run.Args,
		Env:  jc.lang.Env,
		Files: []*gojudge.File{
			{Src: jc.pkg.InputPath(tc)},
			gojudge.Collector("stdout", jc.d.Cfg.Limits.StdoutMax.U()),
			gojudge.TruncatingCollector("stderr", jc.d.Cfg.Limits.StderrMax.U()),
		},
		CPULimit:      cpu,
		ClockLimit:    gojudge.Ms(2*tl + 1000),
		MemoryLimit:   gojudge.MiB(mem),
		StackLimit:    gojudge.MiB(mem), // IOI convention: stack = memory limit
		ProcLimit:     jc.lang.RunProcs,
		CopyIn:        jc.runCopyIn(),
		CopyOutCached: []string{"stdout"},
	}
	res, err := jc.runRetry(ctx, gojudge.Request{Cmd: []gojudge.Cmd{cmd}})
	if err != nil {
		return caseRun{}, err
	}
	r := res[0]
	m := verdict.FromRun(r, cpu)
	if m.Unknown {
		jc.d.Metrics.Alert("unknown_status")
		jc.log.Error("unknown go-judge status", "status", r.Status, "testcase", tc)
		return caseRun{}, sysErr("unknown_status", "%s", m.Message)
	}
	cr := caseRun{res: r, mapping: m, stdoutID: r.FileIDs["stdout"]}
	if m.Verdict == verdict.Pending && cr.stdoutID == "" {
		return caseRun{}, sysErr("sandbox_error", "stdout not returned for testcase %s", tc)
	}
	return cr, nil
}

// checked is the result of checking one testcase.
type checked struct {
	Verdict  verdict.Verdict
	Fraction float64
	Message  string
}

// output is the contestant output for checking: either a go-judge file id or inline bytes.
type output struct {
	fileID string
	inline []byte
	has    bool
}

// fetchOutput writes a go-judge file to the job scratch dir and returns its path.
func (jc *jobCtx) fetchOutput(ctx context.Context, id, name string) (string, error) {
	p := filepath.Join(jc.scratch, name)
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := jc.d.GJ.GetFile(ctx, id, f, int64(jc.d.Cfg.Limits.StdoutMax.U())); err != nil {
		return "", &transientError{reason: "sandbox_unavailable", err: err}
	}
	return p, nil
}

// check runs the configured checker on one output (§10.3). outPath is a local copy of the
// output when available (avoids fetching twice).
func (jc *jobCtx) check(ctx context.Context, tc string, out output, outPath string) (checked, error) {
	p := jc.pkg.Problem
	if !p.HasChecker() {
		var rd io.Reader
		switch {
		case out.inline != nil || (out.has && out.fileID == ""):
			rd = bytes.NewReader(out.inline)
		case outPath != "":
			f, err := os.Open(outPath)
			if err != nil {
				return checked{}, err
			}
			defer f.Close()
			rd = f
		default:
			path, err := jc.fetchOutput(ctx, out.fileID, "out-"+tc)
			if err != nil {
				return checked{}, err
			}
			defer os.Remove(path)
			f, err := os.Open(path)
			if err != nil {
				return checked{}, err
			}
			defer f.Close()
			rd = f
		}
		ans, err := os.Open(jc.pkg.AnswerPath(tc))
		if err != nil {
			return checked{}, &judgeError{detail: "answer file: " + err.Error()}
		}
		defer ans.Close()
		o, err := checker.Compare(rd, ans, checker.Options{FloatAbs: p.Checker.FloatAbs, FloatRel: p.Checker.FloatRel})
		if err != nil {
			return checked{}, &judgeError{detail: "default checker: " + err.Error()}
		}
		v := verdict.WA
		if o.Fraction >= 1 {
			v = verdict.AC
		}
		return checked{Verdict: v, Fraction: o.Fraction, Message: o.Message}, nil
	}

	// testlib checker in go-judge.
	outFile := &gojudge.File{FileID: out.fileID}
	if out.fileID == "" {
		outFile = &gojudge.File{Content: gojudge.Str(string(out.inline))}
	}
	lim := jc.d.Cfg.Limits.Checker
	cmd := gojudge.Cmd{
		Args:     []string{"checker", "input", "output", "answer"},
		Files:    []*gojudge.File{gojudge.EmptyStdin(), gojudge.Collector("stdout", lim.Output.U()), gojudge.Collector("stderr", lim.Output.U())},
		CPULimit: gojudge.Dur(lim.CPU.D()), ClockLimit: gojudge.Dur(lim.Wall.D()),
		MemoryLimit: lim.Memory.U(), ProcLimit: lim.Procs,
		CopyIn: map[string]*gojudge.File{
			"checker": {Src: jc.pkg.CheckerPath()},
			"input":   inputFile(jc.pkg, tc),
			"output":  outFile,
			"answer":  answerFile(jc.pkg, tc),
		},
	}
	res, err := jc.runRetry(ctx, gojudge.Request{Cmd: []gojudge.Cmd{cmd}})
	if err != nil {
		return checked{}, err
	}
	t := checker.ParseTestlib(res[0], p.Checker.PointsMax)
	if t.Verdict == verdict.JE {
		jc.d.Metrics.Alert("checker_failed")
		jc.log.Error("checker failed", "testcase", tc, "status", res[0].Status, "exit", res[0].ExitStatus)
		return checked{}, &judgeError{detail: fmt.Sprintf("checker on testcase %s: %s exit %d", tc, res[0].Status, res[0].ExitStatus)}
	}
	return checked{Verdict: t.Verdict, Fraction: t.Fraction, Message: t.Message}, nil
}

func inputFile(p *testdata.Package, tc string) *gojudge.File {
	if _, err := os.Stat(p.InputPath(tc)); err != nil {
		return &gojudge.File{Content: gojudge.Str("")} // output-only problems may have no .in
	}
	return &gojudge.File{Src: p.InputPath(tc)}
}

func answerFile(p *testdata.Package, tc string) *gojudge.File {
	if _, err := os.Stat(p.AnswerPath(tc)); err != nil {
		return &gojudge.File{Content: gojudge.Str("")}
	}
	return &gojudge.File{Src: p.AnswerPath(tc)}
}

// preview returns the sample-case output preview, or nil when not allowed (§12):
// only for sample testcases and only if feedback.preview_samples.
func (jc *jobCtx) previewAllowed(tc string) bool {
	return jc.job.Feedback.PreviewSamples && jc.pkg.Problem.IsSample(tc)
}

func (jc *jobCtx) previewFrom(r io.Reader) (*string, bool) {
	max := int(jc.d.Cfg.Limits.PreviewMax.U())
	b, _ := io.ReadAll(io.LimitReader(r, int64(max)+1))
	s, cut := checker.Truncate(string(b), max)
	s = strings.ToValidUTF8(s, "�")
	return &s, cut
}

// testResult builds the test_result payload.
func (jc *jobCtx) testResult(tc string, v verdict.Verdict, frac float64, msg string, r *gojudge.Result) report.TestResult {
	msg, _ = checker.Truncate(msg, checker.MaxMessage)
	tr := report.TestResult{
		Testcase: tc, Subtasks: jc.pkg.Problem.SubtasksOf(tc), Verdict: string(v),
		Fraction: frac, CheckerMsg: strings.ToValidUTF8(msg, "�"),
	}
	if r != nil {
		tr.TimeMs, tr.MemoryKB = verdict.TimeMs(*r), verdict.MemoryKB(*r)
	}
	return tr
}
