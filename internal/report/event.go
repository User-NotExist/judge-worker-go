// Package report posts judge events to the API, spooling them to disk when the API is
// unreachable and replaying them in seq order (§12).
package report

import (
	"encoding/json"
	"fmt"
	"time"
)

type Event struct {
	EventID      string          `json:"event_id"`
	Seq          int             `json:"seq"`
	Type         string          `json:"type"`
	SubmissionID int64           `json:"submission_id,omitempty"`
	Attempt      int             `json:"attempt"`
	JobID        string          `json:"job_id"`
	WorkerID     string          `json:"worker_id"`
	SentAt       time.Time       `json:"sent_at"`
	Payload      json.RawMessage `json:"payload"`
}

// AckRef identifies the Redis message to XACK/XDEL once `done` is accepted.
type AckRef struct {
	Stream string `json:"stream"`
	ID     string `json:"id"`
}

// Outcome of posting.
type Outcome int

const (
	Accepted   Outcome = iota // 2xx (incl. duplicate)
	Superseded                // 409: attempt superseded → stop the job, do not ack
	Dropped                   // other 4xx: bug → logged, dropped, alerted
	Spooled                   // retries exhausted; on disk for replay
)

func (o Outcome) String() string {
	return [...]string{"accepted", "superseded", "dropped", "spooled"}[o]
}

// EventKey is the idempotency key prefix for a job attempt.
func EventKey(submissionID int64, jobID string, attempt int) string {
	if submissionID > 0 {
		return fmt.Sprintf("%d:%d", submissionID, attempt)
	}
	return fmt.Sprintf("build-%s:%d", jobID, attempt)
}

// Payloads (§12).

type Started struct {
	Slot           int    `json:"slot"`
	GoJudgeVersion string `json:"go_judge_version"`
}

type Compiled struct {
	OK        bool   `json:"ok"`
	TimeMs    uint64 `json:"time_ms"`
	Log       string `json:"log"`
	Truncated bool   `json:"truncated"`
}

type TestResult struct {
	Testcase         string  `json:"testcase"`
	Subtasks         []int   `json:"subtasks"`
	Verdict          string  `json:"verdict"`
	TimeMs           uint64  `json:"time_ms"`
	MemoryKB         uint64  `json:"memory_kb"`
	Fraction         float64 `json:"fraction"`
	CheckerMsg       string  `json:"checker_msg"`
	OutputPreview    *string `json:"output_preview,omitempty"`
	PreviewTruncated bool    `json:"preview_truncated,omitempty"`
}

type Skipped struct {
	Subtask   int      `json:"subtask"`
	Testcases []string `json:"testcases"`
}

type Done struct {
	Verdict            string  `json:"verdict"`
	ScoreMilli         *int64  `json:"score_milli"`
	SubtaskScoresMilli []int64 `json:"subtask_scores_milli"`
	Reason             *string `json:"reason"`
	Detail             string  `json:"detail,omitempty"`
	GoJudgeVersion     string  `json:"go_judge_version"`
	Toolchain          string  `json:"toolchain"`
	DurationMs         int64   `json:"duration_ms"`
}

type Answer struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type BuildDone struct {
	OK        bool     `json:"ok"`
	Reason    string   `json:"reason,omitempty"`
	Testcase  string   `json:"testcase,omitempty"`
	Log       string   `json:"log,omitempty"`
	Answers   []Answer `json:"answers,omitempty"`
	MaxTimeMs uint64   `json:"max_time_ms"`
	ProblemID int64    `json:"problem_id"`
	Version   int64    `json:"version"`
	Detail    string   `json:"detail,omitempty"`
}
