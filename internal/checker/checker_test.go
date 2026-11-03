package checker

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"judge-worker/internal/gojudge"
	"judge-worker/internal/verdict"
)

func f64(v float64) *float64 { return &v }

func cmp(t *testing.T, out, ans string, o Options) Outcome {
	t.Helper()
	r, err := Compare(strings.NewReader(out), strings.NewReader(ans), o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExact(t *testing.T) {
	if r := cmp(t, "1 2 3\n", "1 2 3\n", Options{}); r.Fraction != 1 {
		t.Fatal(r)
	}
	if r := cmp(t, "1 2 4", "1 2 3", Options{}); r.Fraction != 0 || r.Message != "token 3: expected 3, found 4" {
		t.Fatal(r)
	}
}

func TestWhitespaceVariants(t *testing.T) {
	for _, out := range []string{"1\n2\n3", "  1 2\t3  \r\n\n", "1 2 3", "\n\n1\r\n2 3\v\f"} {
		if r := cmp(t, out, "1 2 3\n", Options{}); r.Fraction != 1 {
			t.Fatalf("%q: %v", out, r)
		}
	}
	if r := cmp(t, "", "", Options{}); r.Fraction != 1 {
		t.Fatal(r)
	}
	if r := cmp(t, "  \n", "", Options{}); r.Fraction != 1 {
		t.Fatal(r)
	}
}

func TestExtraMissing(t *testing.T) {
	r := cmp(t, "1 2 3 4", "1 2 3", Options{})
	if r.Fraction != 0 || r.Message != "token 4: expected end of output, found 4" {
		t.Fatal(r)
	}
	r = cmp(t, "1 2", "1 2 3", Options{})
	if r.Fraction != 0 || r.Message != "token 3: expected 3, found end of output" {
		t.Fatal(r)
	}
}

func TestFloatTolerance(t *testing.T) {
	abs := Options{FloatAbs: f64(1e-6)}
	if cmp(t, "0.1000009", "0.1", abs).Fraction != 1 {
		t.Fatal("within abs")
	}
	if cmp(t, "0.1000011", "0.1", abs).Fraction != 0 {
		t.Fatal("outside abs")
	}
	rel := Options{FloatRel: f64(1e-6)}
	if cmp(t, "1000000.9", "1000000", rel).Fraction != 1 {
		t.Fatal("within rel")
	}
	if cmp(t, "1000001.1", "1000000", rel).Fraction != 0 {
		t.Fatal("outside rel")
	}
	both := Options{FloatAbs: f64(1e-6), FloatRel: f64(1e-6)}
	if cmp(t, "nan", "nan", both).Fraction != 1 { // byte-equal tokens match first
		t.Fatal("nan bytes")
	}
	if cmp(t, "inf", "1e400", both).Fraction != 0 {
		t.Fatal("infinite must not match")
	}
	if cmp(t, "1.0", "1", Options{}).Fraction != 0 {
		t.Fatal("exact compare without tolerance")
	}
	if cmp(t, "abc", "abd", both).Fraction != 0 {
		t.Fatal("non-floats")
	}
}

func TestLongTokenTruncatedMessage(t *testing.T) {
	r := cmp(t, strings.Repeat("a", 100), strings.Repeat("b", 100), Options{})
	if r.Fraction != 0 || len(r.Message) > 120 {
		t.Fatal(r)
	}
}

type repeatReader struct{ n int64 }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	k := int64(len(p))
	if k > r.n {
		k = r.n
	}
	for i := int64(0); i < k; i++ {
		p[i] = "1 "[i%2]
	}
	r.n -= k
	return int(k), nil
}

func TestStreaming64MiB(t *testing.T) {
	// 64 MiB of "1 " on both sides matches without loading it all.
	r, err := Compare(&repeatReader{n: MaxRead}, &repeatReader{n: MaxRead}, Options{})
	if err != nil || r.Fraction != 1 {
		t.Fatal(r, err)
	}
	// Output over 64 MiB → WA, not a crash.
	r, err = Compare(&repeatReader{n: MaxRead + 10}, bytes.NewReader([]byte("1")), Options{})
	if err != nil || r.Fraction != 0 {
		t.Fatal(r, err)
	}
}

func res(status string, exit int, stdout, stderr string) gojudge.Result {
	return gojudge.Result{Status: status, ExitStatus: exit, Files: map[string]string{"stdout": stdout, "stderr": stderr}}
}

func TestParseTestlib(t *testing.T) {
	cases := []struct {
		name string
		r    gojudge.Result
		max  float64
		v    verdict.Verdict
		f    float64
	}{
		{"exit 0", res("Accepted", 0, "", "ok 3 numbers"), 1, verdict.AC, 1},
		{"exit 0 float", res("Accepted", 0, "0.25\n", "ok partial"), 1, verdict.PA, 0.25},
		{"exit 0 float 1", res("Accepted", 0, "1", ""), 1, verdict.AC, 1},
		{"exit 0 float 0", res("Accepted", 0, "0", ""), 1, verdict.WA, 0},
		{"exit 0 garbage stdout", res("Accepted", 0, "hello", ""), 1, verdict.AC, 1},
		{"exit 0 float out of range", res("Accepted", 0, "1.5", ""), 1, verdict.AC, 1},
		{"WA", res("Nonzero Exit Status", 1, "", "wrong answer 1st differs"), 1, verdict.WA, 0},
		{"PE", res("Nonzero Exit Status", 2, "", "wrong output format"), 1, verdict.WA, 0},
		{"fail", res("Nonzero Exit Status", 3, "", "FAIL bad answer"), 1, verdict.JE, 0},
		{"points", res("Nonzero Exit Status", 7, "", "points 0.4 partially"), 1, verdict.PA, 0.4},
		{"points max", res("Nonzero Exit Status", 7, "", "points 30"), 100, verdict.PA, 0.3},
		{"points clamp", res("Nonzero Exit Status", 7, "", "points 7"), 1, verdict.AC, 1},
		{"points zero", res("Nonzero Exit Status", 7, "", "points 0"), 1, verdict.WA, 0},
		{"points malformed", res("Nonzero Exit Status", 7, "", "pts 3"), 1, verdict.JE, 0},
		{"exit 4", res("Nonzero Exit Status", 4, "", ""), 1, verdict.JE, 0},
		{"tle", res("Time Limit Exceeded", 0, "", ""), 1, verdict.JE, 0},
		{"signalled", res("Signalled", 11, "", ""), 1, verdict.JE, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseTestlib(c.r, c.max, false)
			if got.Verdict != c.v || (c.v != verdict.JE && got.Fraction != c.f) {
				t.Fatalf("got %+v want %s %v", got, c.v, c.f)
			}
		})
	}
	long := ParseTestlib(res("Nonzero Exit Status", 1, "", "wrong answer "+strings.Repeat("x", 5000)), 1, false)
	if len(long.Message) > MaxMessage || !strings.HasPrefix(long.Message, "wrong answer ") {
		t.Fatal(len(long.Message))
	}
}

func TestTestlibStrictPE(t *testing.T) {
	pe := res("Nonzero Exit Status", 2, "", "wrong output format Expected EOLN")
	if got := ParseTestlib(pe, 1, true); got.Verdict != verdict.PE || got.Fraction != 0 || got.Message != "wrong output format Expected EOLN" {
		t.Fatal(got)
	}
	if got := ParseTestlib(pe, 1, false); got.Verdict != verdict.WA {
		t.Fatal("non-strict exit 2 stays WA", got)
	}
	if got := ParseTestlib(res("Nonzero Exit Status", 1, "", "wa"), 1, true); got.Verdict != verdict.WA {
		t.Fatal("strict exit 1 stays WA", got)
	}
}

func TestStrictLayout(t *testing.T) {
	strict := Options{Strict: true}
	ans := "1 2\n3\n"
	accepted := []string{
		"1 2\n3\n",     // identical
		"1 2\n3",       // missing final newline
		"1 2\r\n3\r\n", // Windows line endings
	}
	for _, out := range accepted {
		if r := cmp(t, out, ans, strict); r.Fraction != 1 || r.PresentationError {
			t.Fatalf("%q: %+v", out, r)
		}
	}
	pe := map[string]string{
		"1  2\n3\n":  `presentation error before token 2: expected " ", found "  "`,
		"1\n2\n3\n":  `presentation error before token 2: expected " ", found "\n"`,
		"1 2 3\n":    `presentation error before token 3: expected "\n", found " "`,
		" 1 2\n3\n":  `presentation error at start of output: expected nothing, found " "`,
		"1 2\n3 \n":  `presentation error at end of output: expected "\n", found " \n"`,
		"1 2\n3\n\n": `presentation error at end of output: expected "\n", found "\n\n"`,
		"1 2\t\n3\n": `presentation error before token 3: expected "\n", found "\t\n"`,
	}
	for out, msg := range pe {
		r := cmp(t, out, ans, strict)
		if r.Fraction != 0 || !r.PresentationError || r.Message != msg {
			t.Fatalf("%q: got %+v, want %q", out, r, msg)
		}
		// Non-strict mode accepts the same output.
		if r := cmp(t, out, ans, Options{}); r.Fraction != 1 {
			t.Fatalf("non-strict %q: %+v", out, r)
		}
	}
	// A wrong token wins over a layout difference: WA, not PE.
	r := cmp(t, "1\n2\n4\n", ans, strict)
	if r.PresentationError || r.Fraction != 0 || r.Message != "token 3: expected 3, found 4" {
		t.Fatal(r)
	}
	// Float tolerance still applies to tokens in strict mode.
	r = cmp(t, "1.0000001 2\n3\n", "1 2\n3\n", Options{Strict: true, FloatAbs: f64(1e-6)})
	if r.Fraction != 1 {
		t.Fatal(r)
	}
	// Long whitespace runs are summarised, not copied.
	r = cmp(t, "1"+strings.Repeat(" ", 100000)+"2\n3\n", ans, strict)
	if !r.PresentationError || !strings.Contains(r.Message, "100000 whitespace chars") {
		t.Fatal(r)
	}
	// Empty answer: empty output or a lone newline is fine, anything else is PE.
	if r := cmp(t, "\n", "", strict); r.Fraction != 1 {
		t.Fatal(r)
	}
	if r := cmp(t, "\n\n", "", strict); !r.PresentationError {
		t.Fatal(r)
	}
}
