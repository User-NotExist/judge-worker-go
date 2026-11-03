package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"judge-worker/internal/gojudge"
	"judge-worker/internal/testdata"
)

// HelperLanguage is the registry entry used for checkers and managers (§10.4).
const HelperLanguage = "cpp20-gcc13"

// HelperCompiler compiles checker/manager sources with testlib in go-judge. The binary is
// cached in the file store only long enough to download it (raw bytes via GET /file;
// copyOut would return it as a JSON string, which is not binary-safe).
func (d *Deps) HelperCompiler() testdata.Compiler {
	return func(ctx context.Context, src []byte) ([]byte, error) {
		l, ok := d.Langs.Get(HelperLanguage)
		if !ok {
			return nil, &testdata.CompileError{Log: "language " + HelperLanguage + " not in registry"}
		}
		name := "helper.cpp"
		cmd := d.compileCmd(l, []string{name}, map[string][]byte{name: src}, "-I/usr/include/testlib")
		var res []gojudge.Result
		var err error
		for try := 0; try < 2; try++ {
			res, err = d.GJ.Run(ctx, gojudge.Request{Cmd: []gojudge.Cmd{cmd}})
			if err != nil {
				return nil, err
			}
			if s := res[0].Status; s != gojudge.StatusInternalError && s != gojudge.StatusFileError {
				break
			}
			for _, id := range res[0].FileIDs {
				_ = d.GJ.DeleteFile(ctx, id)
			}
			if try == 1 {
				return nil, fmt.Errorf("%w: helper compile: %s", gojudge.ErrUnavailable, res[0].Status)
			}
		}
		r := res[0]
		id, ok := r.FileIDs[l.Compile.Artifact]
		defer func() {
			for _, id := range r.FileIDs {
				_ = d.GJ.DeleteFile(context.Background(), id)
			}
		}()
		if r.Status != gojudge.StatusAccepted || !ok {
			log, _ := compileLog(r, 64<<10)
			return nil, &testdata.CompileError{Log: log}
		}
		var buf bytes.Buffer
		if _, err := d.GJ.GetFile(ctx, id, &buf, 256<<20); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// ProbeToolchains runs each language's version_args once and records the first line.
func (d *Deps) ProbeToolchains(ctx context.Context) {
	for _, id := range d.Langs.IDs() {
		l, _ := d.Langs.Get(id)
		if len(l.VersionArgs) == 0 {
			d.SetToolchain(id, l.Display)
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := d.GJ.Run(ctx, gojudge.Request{Cmd: []gojudge.Cmd{{
			Args: l.VersionArgs, Env: l.Env,
			Files:    []*gojudge.File{gojudge.EmptyStdin(), gojudge.Collector("stdout", 4096), gojudge.Collector("stderr", 4096)},
			CPULimit: gojudge.Sec(5), ClockLimit: gojudge.Sec(10), MemoryLimit: gojudge.MiB(512), ProcLimit: 10,
		}}})
		cancel()
		v := l.Display
		if err == nil && res[0].Status == gojudge.StatusAccepted {
			out := res[0].Files["stdout"]
			if strings.TrimSpace(out) == "" {
				out = res[0].Files["stderr"]
			}
			if first, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); first != "" {
				v = first
			}
		}
		d.SetToolchain(id, v)
	}
}
