package pipeline

import (
	"context"
	"fmt"

	"judge-worker/internal/gojudge"
	"judge-worker/internal/verdict"
)

// ExecResult is the outcome of compiling and running one trusted test program
// (self-test and hostile suite, §15 / §19.3).
type ExecResult struct {
	Compiled   bool
	CompileLog string
	Truncated  bool
	Run        gojudge.Result
	Verdict    verdict.Verdict // CE if not compiled; Pending (= "") means the run was Accepted
	Message    string
	Stdout     string // first 64 KiB
}

// Exec compiles src in language langID and runs it once with stdin and the given limits,
// using exactly the same command construction as real submissions.
func (d *Deps) Exec(ctx context.Context, langID, src, stdin string, timeMs, memMb uint64) (ExecResult, error) {
	l, ok := d.Langs.Get(langID)
	if !ok {
		return ExecResult{}, fmt.Errorf("language %s not in registry", langID)
	}
	var cleanup []string
	defer func() {
		for _, id := range cleanup {
			_ = d.GJ.DeleteFile(context.Background(), id)
		}
	}()
	srcs := map[string][]byte{l.SourceName: []byte(src)}
	res, err := d.GJ.Run(ctx, gojudge.Request{Cmd: []gojudge.Cmd{d.compileCmd(l, []string{l.SourceName}, srcs)}})
	if err != nil {
		return ExecResult{}, err
	}
	c := res[0]
	for _, id := range c.FileIDs {
		cleanup = append(cleanup, id)
	}
	out := ExecResult{}
	out.CompileLog, out.Truncated = compileLog(c, int(d.Cfg.Limits.Compile.Output.U()))
	bin, ok := c.FileIDs[l.Compile.Artifact]
	if c.Status != gojudge.StatusAccepted || !ok {
		out.Verdict = verdict.CE
		out.Message = c.Status
		return out, nil
	}
	out.Compiled = true
	cpu := gojudge.Ms(timeMs)
	run := gojudge.Cmd{
		Args: l.Run.Args, Env: l.Env,
		Files: []*gojudge.File{
			{Content: gojudge.Str(stdin)},
			gojudge.Collector("stdout", d.Cfg.Limits.StdoutMax.U()),
			gojudge.TruncatingCollector("stderr", d.Cfg.Limits.StderrMax.U()),
		},
		CPULimit: cpu, ClockLimit: gojudge.Ms(2*timeMs + 1000),
		MemoryLimit: gojudge.MiB(memMb), StackLimit: gojudge.MiB(memMb), ProcLimit: l.RunProcs,
		CopyIn:        map[string]*gojudge.File{l.Compile.Artifact: {FileID: bin}},
		CopyOutCached: []string{"stdout"},
	}
	res, err = d.GJ.Run(ctx, gojudge.Request{Cmd: []gojudge.Cmd{run}})
	if err != nil {
		return out, err
	}
	r := res[0]
	for _, id := range r.FileIDs {
		cleanup = append(cleanup, id)
	}
	out.Run = r
	m := verdict.FromRun(r, cpu)
	out.Verdict, out.Message = m.Verdict, m.Message
	if id, ok := r.FileIDs["stdout"]; ok {
		w := &capWriter{max: 64 << 10}
		_, _ = d.GJ.GetFile(ctx, id, w, 0)
		out.Stdout = string(w.buf)
	}
	return out, nil
}

type capWriter struct {
	buf []byte
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return len(p), nil
}
