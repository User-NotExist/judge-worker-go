// Package checker contains the trusted in-process default token checker and the
// interpretation of testlib checker results (§10.3).
package checker

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// MaxRead caps bytes read from each side (§10.3: never load more than 64 MiB).
const MaxRead = 64 << 20

// MaxToken caps a single token; a longer token is compared by its prefix and fails if
// the two differ anywhere in the compared range.
const maxToken = 1 << 20

type Options struct {
	FloatAbs *float64
	FloatRel *float64
}

type Outcome struct {
	Fraction float64
	Message  string
}

var errTooLarge = errors.New("input exceeds 64 MiB")

type tokenizer struct {
	r     *bufio.Reader
	read  int64
	limit int64
	buf   []byte
}

func newTokenizer(r io.Reader) *tokenizer {
	return &tokenizer{r: bufio.NewReaderSize(r, 64<<10), limit: MaxRead}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// next returns the next token, or io.EOF. The returned slice is valid until the next call.
func (t *tokenizer) next() ([]byte, error) {
	t.buf = t.buf[:0]
	for {
		c, err := t.r.ReadByte()
		if err != nil {
			if err == io.EOF && len(t.buf) > 0 {
				return t.buf, nil
			}
			return nil, err
		}
		t.read++
		if t.read > t.limit {
			return nil, errTooLarge
		}
		if isSpace(c) {
			if len(t.buf) > 0 {
				return t.buf, nil
			}
			continue
		}
		if len(t.buf) < maxToken {
			t.buf = append(t.buf, c)
		}
	}
}

func clip(b []byte) string {
	const n = 32
	if len(b) > n {
		return strconv.Quote(string(b[:n])) + "…"
	}
	return string(b)
}

func floatEq(a, b []byte, o Options) bool {
	if o.FloatAbs == nil && o.FloatRel == nil {
		return false
	}
	x, err1 := strconv.ParseFloat(string(a), 64)
	y, err2 := strconv.ParseFloat(string(b), 64)
	if err1 != nil || err2 != nil || math.IsInf(x, 0) || math.IsNaN(x) || math.IsInf(y, 0) || math.IsNaN(y) {
		return false
	}
	d := math.Abs(x - y)
	if o.FloatAbs != nil && d <= *o.FloatAbs {
		return true
	}
	if o.FloatRel != nil && d <= *o.FloatRel*math.Abs(y) {
		return true
	}
	return false
}

// Compare checks contestant output against the answer token by token.
// expected = answer, found = output. Returns 1.0 or 0.0. An error means the checker
// itself could not run (I/O error) and the testcase is JE.
func Compare(output, answer io.Reader, o Options) (Outcome, error) {
	out := newTokenizer(output)
	ans := newTokenizer(answer)
	var ansTok []byte
	for i := 1; ; i++ {
		a, errA := ans.next()
		if errA != nil && errA != io.EOF {
			if errA == errTooLarge {
				return Outcome{}, fmt.Errorf("answer file: %w", errA)
			}
			return Outcome{}, errA
		}
		ansTok = append(ansTok[:0], a...)
		f, errF := out.next()
		if errF != nil && errF != io.EOF {
			if errF == errTooLarge {
				return Outcome{Fraction: 0, Message: "output exceeds 64 MiB"}, nil
			}
			return Outcome{}, errF
		}
		switch {
		case errA == io.EOF && errF == io.EOF:
			return Outcome{Fraction: 1, Message: fmt.Sprintf("ok %d tokens", i-1)}, nil
		case errA == io.EOF:
			return Outcome{Fraction: 0, Message: fmt.Sprintf("token %d: expected end of output, found %s", i, clip(f))}, nil
		case errF == io.EOF:
			return Outcome{Fraction: 0, Message: fmt.Sprintf("token %d: expected %s, found end of output", i, clip(ansTok))}, nil
		}
		if string(ansTok) == string(f) || floatEq(f, ansTok, o) {
			continue
		}
		return Outcome{Fraction: 0, Message: fmt.Sprintf("token %d: expected %s, found %s", i, clip(ansTok), clip(f))}, nil
	}
}
