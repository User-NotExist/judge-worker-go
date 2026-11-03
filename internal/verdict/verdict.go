// Package verdict maps go-judge results to testcase verdicts (§11.1).
package verdict

import (
	"fmt"
	"syscall"

	"judge-worker/internal/gojudge"
)

type Verdict string

const (
	AC  Verdict = "AC"
	PA  Verdict = "PA"
	WA  Verdict = "WA"
	TLE Verdict = "TLE"
	MLE Verdict = "MLE"
	OLE Verdict = "OLE"
	RE  Verdict = "RE"
	CE  Verdict = "CE"
	SE  Verdict = "SE"
	JE  Verdict = "JE"
	// Pending is returned by FromRun for Accepted runs: the checker decides.
	Pending Verdict = ""
)

// Mapping is the outcome of mapping a contestant run.
type Mapping struct {
	Verdict Verdict
	Message string
	// Retry is set for File Error / Internal Error: the step runner retries once.
	Retry bool
	// Unknown is set for unrecognised status strings (alert).
	Unknown bool
}

// FromRun maps a contestant run result. cpuLimitNs is the cpuLimit that was requested.
func FromRun(r gojudge.Result, cpuLimitNs uint64) Mapping {
	// Contestant stdout over its cap is OLE whatever status go-judge chose (the program is
	// usually killed by the closed pipe and reports a nonzero exit or signal).
	if r.Overflowed("stdout") {
		return Mapping{Verdict: OLE}
	}
	// Only a truncating collector (stderr) overflowed: the output was cut, the run itself
	// is judged as if it completed normally.
	if r.Status == gojudge.StatusOutputLimit && len(r.FileError) > 0 && !r.Overflowed("stdout") && r.Overflowed("stderr") {
		switch {
		case r.ExitStatus != 0:
			r.Status = gojudge.StatusNonzeroExit
		default:
			r.Status = gojudge.StatusAccepted
		}
	}
	switch r.Status {
	case gojudge.StatusAccepted:
		if r.Time > cpuLimitNs { // guard against rounding at the boundary
			return Mapping{Verdict: TLE, Message: fmt.Sprintf("cpu %.2f s", float64(r.Time)/1e9)}
		}
		return Mapping{Verdict: Pending}
	case gojudge.StatusTimeLimit:
		if r.Time >= cpuLimitNs {
			return Mapping{Verdict: TLE, Message: fmt.Sprintf("cpu %.2f s", float64(r.Time)/1e9)}
		}
		return Mapping{Verdict: TLE, Message: fmt.Sprintf("wall %.2f s", float64(r.RunTime)/1e9)}
	case gojudge.StatusMemoryLimit:
		return Mapping{Verdict: MLE, Message: fmt.Sprintf("peak %d MiB", r.Memory>>20)}
	case gojudge.StatusOutputLimit:
		return Mapping{Verdict: OLE}
	case gojudge.StatusNonzeroExit:
		return Mapping{Verdict: RE, Message: fmt.Sprintf("exit code %d", r.ExitStatus)}
	case gojudge.StatusSignalled:
		return Mapping{Verdict: RE, Message: SignalMessage(r.ExitStatus)}
	case gojudge.StatusDangerousSyscall:
		return Mapping{Verdict: RE, Message: "blocked system call"}
	case gojudge.StatusFileError, gojudge.StatusInternalError:
		return Mapping{Verdict: SE, Message: r.Status, Retry: true}
	default:
		return Mapping{Verdict: SE, Message: fmt.Sprintf("unknown status %q", r.Status), Unknown: true}
	}
}

var sigNames = map[int]string{
	1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 4: "SIGILL", 5: "SIGTRAP", 6: "SIGABRT", 7: "SIGBUS",
	8: "SIGFPE", 9: "SIGKILL", 10: "SIGUSR1", 11: "SIGSEGV", 12: "SIGUSR2", 13: "SIGPIPE",
	14: "SIGALRM", 15: "SIGTERM", 24: "SIGXCPU", 25: "SIGXFSZ", 31: "SIGSYS",
}

func SignalMessage(sig int) string {
	if n, ok := sigNames[sig]; ok {
		return fmt.Sprintf("signal %d (%s)", sig, n)
	}
	return fmt.Sprintf("signal %d", sig)
}

// IsSIGPIPE reports whether a Signalled result was SIGPIPE.
func IsSIGPIPE(r gojudge.Result) bool {
	return r.Status == gojudge.StatusSignalled && r.ExitStatus == int(syscall.SIGPIPE)
}

// TimeMs / MemoryKB are the reported numbers (§11.1).
func TimeMs(r gojudge.Result) uint64   { return (r.Time + 500_000) / 1_000_000 }
func MemoryKB(r gojudge.Result) uint64 { return r.Memory / 1024 }
