package pipeline

import (
	"bytes"
	"context"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"judge-worker/internal/checker"
	"judge-worker/internal/gojudge"
	"judge-worker/internal/score"
	"judge-worker/internal/verdict"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// interactiveCase runs contestant and manager connected by two pipes (§10.5).
func (jc *jobCtx) interactiveCase(ctx context.Context) score.Runner {
	return func(tc string) (score.CaseResult, error) {
		start := time.Now()
		tl := jc.job.Limits.TimeMs
		mem := jc.job.Limits.MemoryMb
		cpu := gojudge.Ms(tl)
		stderrMax := uint64(4 << 10)
		req := gojudge.Request{
			Cmd: []gojudge.Cmd{
				{ // 0: contestant
					Args:        jc.lang.Run.Args,
					Env:         jc.lang.Env,
					Files:       []*gojudge.File{nil, nil, gojudge.TruncatingCollector("stderr", stderrMax)},
					CPULimit:    cpu,
					ClockLimit:  gojudge.Ms(2*tl + 1000),
					MemoryLimit: gojudge.MiB(mem),
					StackLimit:  gojudge.MiB(mem),
					ProcLimit:   jc.lang.RunProcs,
					CopyIn:      jc.runCopyIn(),
				},
				{ // 1: manager (trusted, generous limits)
					Args:        []string{"manager", "input"},
					Files:       []*gojudge.File{nil, nil, gojudge.TruncatingCollector("stderr", stderrMax)},
					CPULimit:    gojudge.Ms(tl*2 + 5000),
					ClockLimit:  gojudge.Ms(tl*3 + 10000),
					MemoryLimit: gojudge.GiB(1),
					ProcLimit:   1,
					CopyIn: map[string]*gojudge.File{
						"manager": {Src: jc.pkg.ManagerPath()},
						"input":   {Src: jc.pkg.InputPath(tc)},
					},
					CopyOut: []string{"score.txt"},
				},
			},
			PipeMapping: []gojudge.PipeMap{
				{In: gojudge.PipeEnd{Index: 0, Fd: 1}, Out: gojudge.PipeEnd{Index: 1, Fd: 0}}, // contestant stdout → manager stdin
				{In: gojudge.PipeEnd{Index: 1, Fd: 1}, Out: gojudge.PipeEnd{Index: 0, Fd: 0}}, // manager stdout → contestant stdin
			},
		}
		res, err := jc.runRetry(ctx, req)
		if err != nil {
			return score.CaseResult{}, err
		}
		c, m := res[0], res[1]
		v, frac, msg, err := interpretInteractive(c, m, cpu)
		if err != nil {
			jc.d.Metrics.Alert("manager_failed")
			jc.log.Error("manager failed", "testcase", tc, "status", m.Status, "exit", m.ExitStatus)
			return score.CaseResult{}, err
		}
		tr := jc.testResult(tc, v, frac, msg, &c)
		return jc.caseDone(ctx, start, tr, v, frac)
	}
}

// interpretInteractive implements the verdict rules of §10.5.
func interpretInteractive(c, m gojudge.Result, cpuLimit uint64) (verdict.Verdict, float64, string, error) {
	cm := verdict.FromRun(c, cpuLimit)
	if cm.Unknown {
		return "", 0, "", sysErr("unknown_status", "%s", cm.Message)
	}
	managerMsg, _ := checker.Truncate(strings.TrimSpace(m.Files["stderr"]), checker.MaxMessage)
	frac, managerOK := managerFraction(m)

	if cm.Verdict != verdict.Pending {
		// Contestant failed. If the manager had already decided WA and the contestant
		// then died of SIGPIPE, report WA; otherwise the contestant's verdict wins.
		if managerOK && frac == 0 && verdict.IsSIGPIPE(c) {
			return verdict.WA, 0, managerMsg, nil
		}
		return cm.Verdict, 0, cm.Message, nil
	}
	if !managerOK {
		return "", 0, "", &judgeError{detail: "manager: " + m.Status + " exit " + strconv.Itoa(m.ExitStatus)}
	}
	return checker.FromFraction(frac), frac, managerMsg, nil
}

// managerFraction: exit 0 and a fraction 0.0–1.0 on the first line of score.txt.
func managerFraction(m gojudge.Result) (float64, bool) {
	if m.Status != gojudge.StatusAccepted || m.ExitStatus != 0 {
		return 0, false
	}
	s, ok := m.Files["score.txt"]
	if !ok {
		return 0, false
	}
	first, _, _ := strings.Cut(s, "\n")
	f, err := strconv.ParseFloat(strings.TrimSpace(first), 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
		return 0, false
	}
	return f, true
}
