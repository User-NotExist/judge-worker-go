// Package checker contains the trusted in-process default token checker and the
// interpretation of testlib checker results.
package checker

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// MaxRead caps bytes read from each side: never load more than 64 MiB.
const MaxRead = 64 << 20

// MaxToken caps a single token; a longer token is compared by its prefix and fails if
// the two differ anywhere in the compared range.
const maxToken = 1 << 20

type Options struct {
	FloatAbs *float64
	FloatRel *float64
	// Strict also compares the whitespace layout with the answer file once every token
	// matches: same separators between tokens, no extra leading or trailing whitespace.
	// "\r\n" counts as "\n", and the output may end with or without one final newline
	// when the answer does.
	Strict bool
}

type Outcome struct {
	Fraction float64
	Message  string
	// PresentationError: all tokens matched but the layout did not (only in Strict mode).
	PresentationError bool
}

// sep summarises one whitespace run (CRLF normalised to LF) in constant memory:
// length + FNV-1a hash, plus the first bytes for messages.
type sep struct {
	n    int64
	h    uint64
	head []byte
}

const sepHead = 32

func newSep() sep { return sep{h: fnvOffset} }

const fnvOffset = 14695981039346656037

func (s *sep) add(c byte) {
	s.n++
	s.h ^= uint64(c)
	s.h *= 1099511628211
	if len(s.head) < sepHead {
		s.head = append(s.head, c)
	}
}

func (s sep) equal(o sep) bool { return s.n == o.n && s.h == o.h }

// isNewlineOrEmpty: "" or a single "\n" (allowed interchangeably at the end of output).
func (s sep) isNewlineOrEmpty() bool { return s.n == 0 || (s.n == 1 && s.head[0] == '\n') }

func (s sep) String() string {
	if s.n == 0 {
		return "nothing"
	}
	q := strconv.Quote(string(s.head))
	if s.n > int64(len(s.head)) {
		return fmt.Sprintf("%s… (%d whitespace chars)", q, s.n)
	}
	return q
}

var errTooLarge = errors.New("input exceeds 64 MiB")

type tokenizer struct {
	r     *bufio.Reader
	read  int64
	limit int64
	buf   []byte
	// before is the whitespace run in front of the token last returned by next; after
	// io.EOF it is the trailing whitespace of the input.
	before sep
	cur    sep
}

func newTokenizer(r io.Reader) *tokenizer {
	return &tokenizer{r: bufio.NewReaderSize(r, 64<<10), limit: MaxRead, cur: newSep(), before: newSep()}
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
			if err == io.EOF {
				t.before = t.cur // trailing whitespace
			}
			return nil, err
		}
		t.read++
		if t.read > t.limit {
			return nil, errTooLarge
		}
		if isSpace(c) {
			if c == '\r' {
				if nx, err := t.r.Peek(1); err == nil && nx[0] == '\n' {
					continue // CRLF counts as LF
				}
			}
			t.cur.add(c)
			if len(t.buf) > 0 {
				return t.buf, nil
			}
			continue
		}
		if len(t.buf) == 0 {
			t.before, t.cur = t.cur, newSep()
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
//
// In Strict mode a wrong token is still reported first (WA); a layout difference is
// reported as a presentation error only when every token matched.
func Compare(output, answer io.Reader, o Options) (Outcome, error) {
	out := newTokenizer(output)
	ans := newTokenizer(answer)
	var ansTok []byte
	pe := ""
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
			if o.Strict && pe == "" && !out.before.equal(ans.before) &&
				!(out.before.isNewlineOrEmpty() && ans.before.isNewlineOrEmpty()) {
				pe = fmt.Sprintf("presentation error at end of output: expected %s, found %s", ans.before, out.before)
			}
			if pe != "" {
				return Outcome{Fraction: 0, Message: pe, PresentationError: true}, nil
			}
			return Outcome{Fraction: 1, Message: fmt.Sprintf("ok %d tokens", i-1)}, nil
		case errA == io.EOF:
			return Outcome{Fraction: 0, Message: fmt.Sprintf("token %d: expected end of output, found %s", i, clip(f))}, nil
		case errF == io.EOF:
			return Outcome{Fraction: 0, Message: fmt.Sprintf("token %d: expected %s, found end of output", i, clip(ansTok))}, nil
		}
		if string(ansTok) == string(f) || floatEq(f, ansTok, o) {
			if o.Strict && pe == "" && !out.before.equal(ans.before) {
				where := fmt.Sprintf("before token %d", i)
				if i == 1 {
					where = "at start of output"
				}
				pe = fmt.Sprintf("presentation error %s: expected %s, found %s", where, ans.before, out.before)
			}
			continue
		}
		return Outcome{Fraction: 0, Message: fmt.Sprintf("token %d: expected %s, found %s", i, clip(ansTok), clip(f))}, nil
	}
}
