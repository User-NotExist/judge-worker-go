package verdict

import (
	"testing"

	"judge-worker/internal/gojudge"
)

func TestFromRun(t *testing.T) {
	limit := gojudge.Ms(1000)
	cases := []struct {
		name  string
		r     gojudge.Result
		want  Verdict
		msg   string
		retry bool
		unk   bool
	}{
		{"accepted", gojudge.Result{Status: "Accepted", Time: gojudge.Ms(500)}, Pending, "", false, false},
		{"accepted at limit", gojudge.Result{Status: "Accepted", Time: limit}, Pending, "", false, false},
		{"accepted over limit guard", gojudge.Result{Status: "Accepted", Time: limit + 1}, TLE, "cpu 1.00 s", false, false},
		{"tle cpu", gojudge.Result{Status: "Time Limit Exceeded", Time: gojudge.Ms(1020)}, TLE, "cpu 1.02 s", false, false},
		{"tle wall", gojudge.Result{Status: "Time Limit Exceeded", Time: gojudge.Ms(10), RunTime: gojudge.Ms(3000)}, TLE, "wall 3.00 s", false, false},
		{"mle", gojudge.Result{Status: "Memory Limit Exceeded", Memory: 257 << 20}, MLE, "peak 257 MiB", false, false},
		{"ole", gojudge.Result{Status: "Output Limit Exceeded"}, OLE, "", false, false},
		{"nonzero", gojudge.Result{Status: "Nonzero Exit Status", ExitStatus: 3}, RE, "exit code 3", false, false},
		{"signalled", gojudge.Result{Status: "Signalled", ExitStatus: 11}, RE, "signal 11 (SIGSEGV)", false, false},
		{"syscall", gojudge.Result{Status: "Dangerous Syscall"}, RE, "blocked system call", false, false},
		{"file error", gojudge.Result{Status: "File Error"}, SE, "File Error", true, false},
		{"internal", gojudge.Result{Status: "Internal Error"}, SE, "Internal Error", true, false},
		{"unknown", gojudge.Result{Status: "Weird"}, SE, `unknown status "Weird"`, false, true},
		{"stdout overflow as exit 120", gojudge.Result{Status: "Nonzero Exit Status", ExitStatus: 120, FileError: []gojudge.FileError{{Name: "stdout", Type: "CollectSizeExceeded"}}}, OLE, "", false, false},
		{"stdout overflow as signal", gojudge.Result{Status: "Signalled", ExitStatus: 13, FileError: []gojudge.FileError{{Name: "stdout", Type: "CollectSizeExceeded"}}}, OLE, "", false, false},
		{"stderr overflow only, exit 0", gojudge.Result{Status: "Output Limit Exceeded", FileError: []gojudge.FileError{{Name: "stderr", Type: "CollectSizeExceeded"}}}, Pending, "", false, false},
		{"stderr overflow only, exit 2", gojudge.Result{Status: "Output Limit Exceeded", ExitStatus: 2, FileError: []gojudge.FileError{{Name: "stderr", Type: "CollectSizeExceeded"}}}, RE, "exit code 2", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := FromRun(c.r, limit)
			if m.Verdict != c.want || m.Message != c.msg || m.Retry != c.retry || m.Unknown != c.unk {
				t.Fatalf("got %+v, want %s %q retry=%v unknown=%v", m, c.want, c.msg, c.retry, c.unk)
			}
		})
	}
}

func TestNumbers(t *testing.T) {
	r := gojudge.Result{Time: 1_499_999, Memory: 4120 * 1024}
	if TimeMs(r) != 1 || MemoryKB(r) != 4120 {
		t.Fatal(TimeMs(r), MemoryKB(r))
	}
	if TimeMs(gojudge.Result{Time: 1_500_000}) != 2 {
		t.Fatal("rounding")
	}
	if !IsSIGPIPE(gojudge.Result{Status: "Signalled", ExitStatus: 13}) {
		t.Fatal("sigpipe")
	}
}
