package testdata

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"judge-worker/internal/job"
	"judge-worker/internal/score"
)

type ProblemType string

const (
	Batch       ProblemType = "batch"
	Interactive ProblemType = "interactive"
	OutputOnly  ProblemType = "output_only"
)

type Testcase struct {
	Name   string `yaml:"name"`
	Sample bool   `yaml:"sample"`
}

type Subtask struct {
	Index     int      `yaml:"index"`
	Points    *int64   `yaml:"points"`
	Mode      string   `yaml:"mode"`
	Testcases []string `yaml:"testcases"`
	DependsOn []int    `yaml:"depends_on"`
}

type Problem struct {
	Type   ProblemType `yaml:"type"`
	Limits struct {
		MemoryMb uint64            `yaml:"memory_mb"`
		TimeMs   map[string]uint64 `yaml:"time_ms"`
	} `yaml:"limits"`
	Checker struct {
		Kind      string   `yaml:"kind"`
		FloatAbs  *float64 `yaml:"float_abs"`
		FloatRel  *float64 `yaml:"float_rel"`
		PointsMax float64  `yaml:"points_max"` // denominator for testlib "points X"; default 1
	} `yaml:"checker"`
	Testcases []Testcase `yaml:"testcases"`
	Subtasks  []Subtask  `yaml:"subtasks"`
}

func (p *Problem) HasChecker() bool { return p.Checker.Kind == "testlib" }
func (p *Problem) HasManager() bool { return p.Type == Interactive }

func (p *Problem) Order() []string {
	out := make([]string, len(p.Testcases))
	for i, tc := range p.Testcases {
		out[i] = tc.Name
	}
	return out
}

func (p *Problem) IsSample(name string) bool {
	for _, tc := range p.Testcases {
		if tc.Name == name {
			return tc.Sample
		}
	}
	return false
}

// SubtasksOf returns the indexes of subtasks containing tc.
func (p *Problem) SubtasksOf(tc string) []int {
	out := []int{}
	for _, st := range p.Subtasks {
		for _, t := range st.Testcases {
			if t == tc {
				out = append(out, st.Index)
				break
			}
		}
	}
	return out
}

func (p *Problem) ScoreSubtasks() []score.Subtask {
	out := make([]score.Subtask, len(p.Subtasks))
	for i, st := range p.Subtasks {
		out[i] = score.Subtask{Index: st.Index, Points: *st.Points, Mode: score.Mode(st.Mode), Testcases: st.Testcases, DependsOn: st.DependsOn}
	}
	return out
}

// ValidateOpts carries the per-job context for validation.
type ValidateOpts struct {
	Language string // job language; "" skips the time-limit check (build of staging package)
	// RequireAnswers is false for build jobs (staging packages may lack .ans).
	RequireAnswers bool
}

// ParseProblem reads problem.yaml from dir and validates the structural rules of §6.3
// that do not depend on the job.
func ParseProblem(dir string) (*Problem, error) {
	b, err := os.ReadFile(filepath.Join(dir, "problem.yaml"))
	if err != nil {
		return nil, fmt.Errorf("problem.yaml: %w", err)
	}
	var p Problem
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("problem.yaml: %w", err)
	}
	if err := p.validateStatic(dir); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Problem) validateStatic(dir string) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	switch p.Type {
	case Batch, Interactive, OutputOnly:
	default:
		add("type %q invalid", p.Type)
	}
	if p.Checker.Kind == "" {
		p.Checker.Kind = "default"
	}
	switch p.Checker.Kind {
	case "default", "testlib":
	default:
		add("checker.kind %q invalid", p.Checker.Kind)
	}
	if p.Checker.Kind == "testlib" && !exists(filepath.Join(dir, "checker", "checker.cpp")) {
		add("checker/checker.cpp missing for kind testlib")
	}
	if p.Type == Interactive && !exists(filepath.Join(dir, "manager", "manager.cpp")) {
		add("manager/manager.cpp missing for interactive problem")
	}
	if len(p.Testcases) == 0 {
		add("no testcases")
	}
	names := map[string]bool{}
	for _, tc := range p.Testcases {
		if !job.ValidName(tc.Name) {
			add("testcase name %q invalid", tc.Name)
			continue
		}
		if names[tc.Name] {
			add("testcase %q declared twice", tc.Name)
		}
		names[tc.Name] = true
		if p.Type != OutputOnly && !exists(filepath.Join(dir, "tests", tc.Name+".in")) {
			add("tests/%s.in missing", tc.Name)
		}
	}
	if len(p.Subtasks) == 0 {
		add("no subtasks")
	}
	seenIdx := map[int]int{}
	for i, st := range p.Subtasks {
		if _, dup := seenIdx[st.Index]; dup {
			add("subtask index %d duplicated", st.Index)
		}
		seenIdx[st.Index] = i
		if st.Points == nil || *st.Points < 0 {
			add("subtask %d: points must be a non-negative integer", st.Index)
		}
		if st.Mode == "" {
			p.Subtasks[i].Mode = string(score.ModeMin)
		} else if st.Mode != string(score.ModeMin) && st.Mode != string(score.ModeSum) {
			add("subtask %d: mode %q invalid", st.Index, st.Mode)
		}
		if len(st.Testcases) == 0 {
			add("subtask %d has no testcases", st.Index)
		}
		for _, tc := range st.Testcases {
			if !names[tc] {
				add("subtask %d: testcase %q does not exist", st.Index, tc)
			}
		}
		for _, d := range st.DependsOn {
			j, ok := seenIdx[d]
			if !ok || j >= i {
				add("subtask %d: depends_on %d must refer to an earlier subtask", st.Index, d)
			}
		}
	}
	if p.Checker.FloatAbs != nil && *p.Checker.FloatAbs < 0 || p.Checker.FloatRel != nil && *p.Checker.FloatRel < 0 {
		add("float tolerances must be non-negative")
	}
	if len(errs) > 0 {
		return fmt.Errorf("problem.yaml invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ValidateFor applies the job-dependent rules of §6.3.
func (p *Problem) ValidateFor(dir string, o ValidateOpts) error {
	var errs []string
	if o.Language != "" && o.Language != job.OutputOnly && p.Type != OutputOnly {
		if _, ok := p.Limits.TimeMs[o.Language]; !ok {
			errs = append(errs, fmt.Sprintf("limits.time_ms has no entry for %s", o.Language))
		}
	}
	// .ans needed unless interactive (the manager decides) — and in build jobs.
	if o.RequireAnswers && p.Type != Interactive {
		for _, tc := range p.Testcases {
			if !exists(filepath.Join(dir, "tests", tc.Name+".ans")) {
				errs = append(errs, fmt.Sprintf("tests/%s.ans missing", tc.Name))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("problem invalid for job: %s", strings.Join(errs, "; "))
	}
	return nil
}

func exists(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular()
}
