package pipeline

import (
	"errors"
	"testing"

	"judge-worker/internal/gojudge"
	"judge-worker/internal/verdict"
)

func mgr(status string, exit int, score string) gojudge.Result {
	files := map[string]string{"stderr": "msg"}
	if score != "-" {
		files["score.txt"] = score
	}
	return gojudge.Result{Status: status, ExitStatus: exit, Files: files}
}

func TestInterpretInteractive(t *testing.T) {
	ok := gojudge.Result{Status: "Accepted", Time: gojudge.Ms(10)}
	tle := gojudge.Result{Status: "Time Limit Exceeded", Time: gojudge.Ms(1100)}
	pipe := gojudge.Result{Status: "Signalled", ExitStatus: 13}
	limit := gojudge.Ms(1000)
	cases := []struct {
		name string
		c, m gojudge.Result
		v    verdict.Verdict
		f    float64
		je   bool
	}{
		{"correct", ok, mgr("Accepted", 0, "1.0\n"), verdict.AC, 1, false},
		{"partial", ok, mgr("Accepted", 0, "0.5"), verdict.PA, 0.5, false},
		{"wrong", ok, mgr("Accepted", 0, "0"), verdict.WA, 0, false},
		{"contestant tle wins", tle, mgr("Accepted", 0, "1"), verdict.TLE, 0, false},
		{"contestant tle, manager killed", tle, mgr("Signalled", 13, "-"), verdict.TLE, 0, false},
		{"manager WA then SIGPIPE", pipe, mgr("Accepted", 0, "0"), verdict.WA, 0, false},
		{"SIGPIPE without manager WA", pipe, mgr("Nonzero Exit Status", 1, "-"), verdict.RE, 0, false},
		{"manager crashed", ok, mgr("Nonzero Exit Status", 3, "-"), "", 0, true},
		{"manager bad score", ok, mgr("Accepted", 0, "1.7"), "", 0, true},
		{"manager no score", ok, mgr("Accepted", 0, "-"), "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, f, _, err := interpretInteractive(c.c, c.m, limit)
			var je *judgeError
			if c.je {
				if !errors.As(err, &je) {
					t.Fatalf("want JE, got %v %v", v, err)
				}
				return
			}
			if err != nil || v != c.v || f != c.f {
				t.Fatalf("got %s %v %v", v, f, err)
			}
		})
	}
}
