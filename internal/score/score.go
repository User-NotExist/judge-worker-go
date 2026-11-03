// Package score implements subtask scoring with early stop and dependencies (§11.2).
// Fractions are float64 per testcase; points are integer milli-points.
package score

import (
	"math"

	"judge-worker/internal/verdict"
)

type Mode string

const (
	ModeMin Mode = "min"
	ModeSum Mode = "sum" // proportional: points × mean(fractions)
)

type Subtask struct {
	Index     int
	Points    int64
	Mode      Mode
	Testcases []string
	DependsOn []int
}

type CaseResult struct {
	Verdict  verdict.Verdict
	Fraction float64
}

// Runner runs and checks one testcase. A non-nil error aborts scoring (SE / JE / cancel).
type Runner func(tc string) (CaseResult, error)

// Hooks receive progress notifications (for events). All optional.
type Hooks struct {
	Skipped func(subtask int, testcases []string)
}

type Result struct {
	SubtaskMilli []int64
	TotalMilli   int64
	// Verdict is the first non-AC verdict in execution order, else AC.
	Verdict verdict.Verdict
	// Ran is the execution order of testcases actually run.
	Ran []string
}

// Score runs testcases on demand in the order required by the subtasks. order is the
// problem.yaml declaration order; within a subtask testcases are taken in that order.
func Score(subtasks []Subtask, order []string, run Runner, h Hooks) (Result, error) {
	pos := make(map[string]int, len(order))
	for i, tc := range order {
		pos[tc] = i
	}
	results := map[string]CaseResult{}
	res := Result{SubtaskMilli: make([]int64, len(subtasks)), Verdict: verdict.AC}
	byIndex := map[int]int{} // subtask index -> position
	for i, st := range subtasks {
		byIndex[st.Index] = i
	}

	runOne := func(tc string) (CaseResult, error) {
		if r, ok := results[tc]; ok {
			return r, nil
		}
		r, err := run(tc)
		if err != nil {
			return r, err
		}
		results[tc] = r
		res.Ran = append(res.Ran, tc)
		if res.Verdict == verdict.AC && r.Verdict != verdict.AC {
			res.Verdict = r.Verdict
		}
		return r, nil
	}

	for i, st := range subtasks {
		cases := sortedByDecl(st.Testcases, pos)
		depOK := true
		for _, d := range st.DependsOn {
			j, ok := byIndex[d]
			if !ok || res.SubtaskMilli[j] < subtasks[j].Points*1000 {
				depOK = false
				break
			}
		}
		if !depOK {
			skip(h, st.Index, notRun(cases, results))
			res.SubtaskMilli[i] = 0
			continue
		}
		var fracs []float64
		for k, tc := range cases {
			r, err := runOne(tc)
			if err != nil {
				return res, err
			}
			fracs = append(fracs, r.Fraction)
			if st.Mode != ModeSum && r.Fraction <= 0 {
				skip(h, st.Index, notRun(cases[k+1:], results))
				break
			}
		}
		var f float64
		if st.Mode == ModeSum {
			f = mean(fracs)
		} else {
			f = minOf(fracs)
		}
		res.SubtaskMilli[i] = int64(math.Floor(float64(st.Points*1000)*f + 1e-9))
	}
	for _, s := range res.SubtaskMilli {
		res.TotalMilli += s
	}
	return res, nil
}

func skip(h Hooks, st int, tcs []string) {
	if h.Skipped != nil && len(tcs) > 0 {
		h.Skipped(st, tcs)
	}
}

func notRun(tcs []string, done map[string]CaseResult) []string {
	var out []string
	for _, tc := range tcs {
		if _, ok := done[tc]; !ok {
			out = append(out, tc)
		}
	}
	return out
}

func sortedByDecl(tcs []string, pos map[string]int) []string {
	out := append([]string(nil), tcs...)
	// insertion sort: lists are short
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && pos[out[j]] < pos[out[j-1]]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func minOf(f []float64) float64 {
	if len(f) == 0 {
		return 0
	}
	m := f[0]
	for _, x := range f[1:] {
		m = math.Min(m, x)
	}
	return math.Max(0, math.Min(1, m))
}

func mean(f []float64) float64 {
	if len(f) == 0 {
		return 0
	}
	var s float64
	for _, x := range f {
		s += x
	}
	return math.Max(0, math.Min(1, s/float64(len(f))))
}
