package checker

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"judge-worker/internal/gojudge"
	"judge-worker/internal/verdict"
)

// MaxMessage is the cap on checker messages shown to students (§10.3).
const MaxMessage = 1 << 10

// TestlibResult is the interpretation of one testlib checker run.
type TestlibResult struct {
	Verdict  verdict.Verdict
	Fraction float64
	Message  string
}

// Truncate cuts s to n bytes on a rune boundary.
func Truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
}

// FromFraction maps a fraction to AC / PA / WA.
func FromFraction(f float64) verdict.Verdict {
	switch {
	case f >= 1:
		return verdict.AC
	case f <= 0:
		return verdict.WA
	default:
		return verdict.PA
	}
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

// ParseTestlib interprets a testlib checker result.
// pointsMax is the denominator for "points X" (problem.yaml checker.points_max, default 1).
// strict reports testlib's presentation error (exit 2) as PE instead of WA.
func ParseTestlib(r gojudge.Result, pointsMax float64, strict bool) TestlibResult {
	stderr := r.Files["stderr"]
	stdout := strings.TrimSpace(r.Files["stdout"])
	msg, _ := Truncate(strings.TrimSpace(stderr), MaxMessage)
	if pointsMax <= 0 {
		pointsMax = 1
	}

	judgeErr := func() TestlibResult {
		return TestlibResult{Verdict: verdict.JE, Message: msg}
	}

	switch r.Status {
	case gojudge.StatusAccepted:
		// exit 0: AC, unless stdout is a float in [0,1] (partial score).
		if stdout != "" {
			if f, err := strconv.ParseFloat(stdout, 64); err == nil && !math.IsNaN(f) && f >= 0 && f <= 1 {
				return TestlibResult{Verdict: FromFraction(f), Fraction: f, Message: msg}
			}
		}
		return TestlibResult{Verdict: verdict.AC, Fraction: 1, Message: msg}
	case gojudge.StatusNonzeroExit:
		switch r.ExitStatus {
		case 1:
			return TestlibResult{Verdict: verdict.WA, Fraction: 0, Message: msg}
		case 2:
			if strict {
				return TestlibResult{Verdict: verdict.PE, Fraction: 0, Message: msg}
			}
			return TestlibResult{Verdict: verdict.WA, Fraction: 0, Message: msg}
		case 7:
			if x, ok := parsePoints(stderr); ok {
				f := clamp01(x / pointsMax)
				return TestlibResult{Verdict: FromFraction(f), Fraction: f, Message: msg}
			}
			return judgeErr()
		default: // 3 = _fail, anything else
			return judgeErr()
		}
	default:
		return judgeErr()
	}
}

// parsePoints reads "points X" at the start of stderr.
func parsePoints(stderr string) (float64, bool) {
	s := strings.TrimLeft(stderr, " \t\r\n")
	if !strings.HasPrefix(s, "points ") {
		return 0, false
	}
	fields := strings.Fields(strings.TrimPrefix(s, "points "))
	if len(fields) == 0 {
		return 0, false
	}
	x, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, false
	}
	return x, true
}
