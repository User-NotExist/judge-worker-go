// Package job defines the JudgeJob / BuildJob contract (§6).
package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

const OutputOnly = "output-only"

type FileRef struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type ProblemRef struct {
	ID            int64  `json:"id"`
	Version       int64  `json:"version"`
	PackageSHA256 string `json:"package_sha256"`
	PackageKey    string `json:"package_key"`
}

type Limits struct {
	TimeMs   uint64 `json:"time_ms"`
	MemoryMb uint64 `json:"memory_mb"`
}

// Job is the union of JudgeJob (kind=judge) and BuildJob (kind=build).
type Job struct {
	Kind         string `json:"kind"`
	JobID        string `json:"job_id"`
	SubmissionID int64  `json:"submission_id,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	CourseID     int64  `json:"course_id,omitempty"`
	Lane         string `json:"lane,omitempty"`
	Language     string `json:"language,omitempty"`
	Source       struct {
		Files []FileRef `json:"files"`
	} `json:"source"`
	Problem  ProblemRef `json:"problem"`
	Limits   Limits     `json:"limits"`
	Feedback struct {
		PreviewSamples bool `json:"preview_samples"`
	} `json:"feedback"`
	CreatedAt time.Time `json:"created_at"`

	// Build-only.
	Reference *struct {
		Language string    `json:"language"`
		Files    []FileRef `json:"files"`
	} `json:"reference,omitempty"`
	AnswersUpload map[string]string `json:"answers_upload,omitempty"`
}

// Partial carries whatever identifying fields could be decoded from a malformed job,
// so the worker can still report `done{SE, bad_job}` (§7.2).
type Partial struct {
	Kind         string `json:"kind"`
	JobID        string `json:"job_id"`
	SubmissionID int64  `json:"submission_id"`
	Attempt      int    `json:"attempt"`
}

var (
	shaRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

func Parse(data []byte) (*Job, error) {
	var j Job
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("decode job: %w", err)
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return &j, nil
}

func ParsePartial(data []byte) Partial {
	var p Partial
	_ = json.Unmarshal(data, &p)
	return p
}

func ValidName(n string) bool {
	return nameRe.MatchString(n) && n != "." && n != ".." && path.Base(n) == n
}

func validFiles(fs []FileRef) error {
	if len(fs) == 0 {
		return errors.New("no source files")
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !ValidName(f.Name) {
			return fmt.Errorf("invalid file name %q", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate file %q", f.Name)
		}
		seen[f.Name] = true
		if f.URL == "" {
			return fmt.Errorf("file %q has no url", f.Name)
		}
		if !shaRe.MatchString(f.SHA256) {
			return fmt.Errorf("file %q has invalid sha256", f.Name)
		}
	}
	return nil
}

func (j *Job) Validate() error {
	if j.JobID == "" {
		return errors.New("job_id missing")
	}
	if !shaRe.MatchString(j.Problem.PackageSHA256) {
		return errors.New("problem.package_sha256 invalid")
	}
	if j.Problem.PackageKey == "" {
		return errors.New("problem.package_key missing")
	}
	if j.Limits.TimeMs == 0 || j.Limits.TimeMs > 60_000 {
		return errors.New("limits.time_ms must be 1..60000")
	}
	if j.Limits.MemoryMb == 0 || j.Limits.MemoryMb > 8192 {
		return errors.New("limits.memory_mb must be 1..8192")
	}
	switch j.Kind {
	case "judge":
		if j.SubmissionID <= 0 {
			return errors.New("submission_id missing")
		}
		if j.Attempt < 1 {
			return errors.New("attempt must be >= 1")
		}
		if j.Language == "" {
			return errors.New("language missing")
		}
		if err := validFiles(j.Source.Files); err != nil {
			return err
		}
	case "build":
		if j.Reference == nil || j.Reference.Language == "" {
			return errors.New("reference missing")
		}
		if err := validFiles(j.Reference.Files); err != nil {
			return err
		}
		if len(j.AnswersUpload) == 0 {
			return errors.New("answers_upload missing")
		}
		if j.Attempt < 1 {
			j.Attempt = 1
		}
	default:
		return fmt.Errorf("unknown kind %q", j.Kind)
	}
	return nil
}
