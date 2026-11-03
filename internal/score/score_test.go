package score

import (
	"errors"
	"reflect"
	"testing"

	"judge-worker/internal/verdict"
)

func fixed(m map[string]float64) (Runner, *[]string) {
	var calls []string
	return func(tc string) (CaseResult, error) {
		calls = append(calls, tc)
		f := m[tc]
		v := verdict.WA
		switch {
		case f >= 1:
			v = verdict.AC
		case f > 0:
			v = verdict.PA
		}
		return CaseResult{Verdict: v, Fraction: f}, nil
	}, &calls
}

// The example from problem.yaml in §6.3.
var spec = []Subtask{
	{Index: 0, Points: 0, Mode: ModeMin, Testcases: []string{"1"}},
	{Index: 1, Points: 30, Mode: ModeMin, Testcases: []string{"2", "3"}},
	{Index: 2, Points: 70, Mode: ModeMin, Testcases: []string{"2", "3", "4"}, DependsOn: []int{1}},
}
var order = []string{"1", "2", "3", "4"}

func TestAllFull(t *testing.T) {
	run, calls := fixed(map[string]float64{"1": 1, "2": 1, "3": 1, "4": 1})
	r, err := Score(spec, order, run, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalMilli != 100000 || r.Verdict != verdict.AC || !reflect.DeepEqual(r.SubtaskMilli, []int64{0, 30000, 70000}) {
		t.Fatalf("%+v", r)
	}
	// Shared testcases run once per job.
	if !reflect.DeepEqual(*calls, []string{"1", "2", "3", "4"}) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestAllZero(t *testing.T) {
	run, calls := fixed(map[string]float64{})
	var skipped []Skipped
	r, _ := Score(spec, order, run, Hooks{Skipped: func(st int, tcs []string) { skipped = append(skipped, Skipped{st, tcs}) }})
	if r.TotalMilli != 0 || r.Verdict != verdict.WA {
		t.Fatalf("%+v", r)
	}
	// Subtask 1 stops at "2"; subtask 2 depends on 1 → skipped entirely (3 and 4 never run).
	if !reflect.DeepEqual(*calls, []string{"1", "2"}) {
		t.Fatalf("calls %v", *calls)
	}
	want := []Skipped{{1, []string{"3"}}, {2, []string{"3", "4"}}}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("skipped %v", skipped)
	}
}

type Skipped struct {
	St  int
	Tcs []string
}

func TestEarlyStopThenRunLater(t *testing.T) {
	subs := []Subtask{
		{Index: 0, Points: 40, Mode: ModeMin, Testcases: []string{"1", "2"}},
		{Index: 1, Points: 60, Mode: ModeMin, Testcases: []string{"2", "3"}},
	}
	run, calls := fixed(map[string]float64{"1": 0, "2": 1, "3": 1})
	r, _ := Score(subs, []string{"1", "2", "3"}, run, Hooks{})
	// "2" skipped in subtask 0 but run for subtask 1.
	if !reflect.DeepEqual(*calls, []string{"1", "2", "3"}) || r.TotalMilli != 60000 {
		t.Fatalf("calls %v %+v", *calls, r)
	}
	if r.Verdict != verdict.WA {
		t.Fatal(r.Verdict)
	}
}

func TestSumModeAndRounding(t *testing.T) {
	subs := []Subtask{{Index: 0, Points: 10, Mode: ModeSum, Testcases: []string{"a", "b", "c"}}}
	run, calls := fixed(map[string]float64{"a": 1, "b": 0, "c": 1})
	r, _ := Score(subs, []string{"a", "b", "c"}, run, Hooks{})
	// 10 × 2/3 × 1000 = 6666.67 → floor 6666 milli-points; no early stop in sum mode.
	if r.TotalMilli != 6666 || len(*calls) != 3 {
		t.Fatalf("%+v %v", r, *calls)
	}
}

func TestPartialMin(t *testing.T) {
	subs := []Subtask{{Index: 0, Points: 100, Mode: ModeMin, Testcases: []string{"a", "b"}}}
	run, _ := fixed(map[string]float64{"a": 0.5, "b": 0.25})
	r, _ := Score(subs, []string{"a", "b"}, run, Hooks{})
	if r.TotalMilli != 25000 || r.Verdict != verdict.PA {
		t.Fatalf("%+v", r)
	}
}

func TestDependencyPartialNotEnough(t *testing.T) {
	subs := []Subtask{
		{Index: 0, Points: 10, Mode: ModeSum, Testcases: []string{"a", "b"}},
		{Index: 1, Points: 90, Mode: ModeMin, Testcases: []string{"c"}, DependsOn: []int{0}},
	}
	run, calls := fixed(map[string]float64{"a": 1, "b": 0.5, "c": 1})
	r, _ := Score(subs, []string{"a", "b", "c"}, run, Hooks{})
	if r.TotalMilli != 7500 || len(*calls) != 2 {
		t.Fatalf("%+v %v", r, *calls)
	}
}

func TestDeclarationOrderWithinSubtask(t *testing.T) {
	subs := []Subtask{{Index: 0, Points: 1, Mode: ModeMin, Testcases: []string{"3", "1", "2"}}}
	run, calls := fixed(map[string]float64{"1": 1, "2": 1, "3": 1})
	_, _ = Score(subs, []string{"1", "2", "3"}, run, Hooks{})
	if !reflect.DeepEqual(*calls, []string{"1", "2", "3"}) {
		t.Fatal(*calls)
	}
}

func TestRunnerErrorAborts(t *testing.T) {
	boom := errors.New("se")
	_, err := Score(spec, order, func(string) (CaseResult, error) { return CaseResult{}, boom }, Hooks{})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
