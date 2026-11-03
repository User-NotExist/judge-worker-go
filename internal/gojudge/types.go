// Package gojudge is a typed HTTP client for the go-judge REST API (§8).
// Units: nanoseconds for cpuLimit / clockLimit, bytes for memory, stack and file sizes.
package gojudge

import "time"

// Status strings, checked against the pinned go-judge version (§22).
const (
	StatusAccepted         = "Accepted"
	StatusMemoryLimit      = "Memory Limit Exceeded"
	StatusTimeLimit        = "Time Limit Exceeded"
	StatusOutputLimit      = "Output Limit Exceeded"
	StatusFileError        = "File Error"
	StatusNonzeroExit      = "Nonzero Exit Status"
	StatusSignalled        = "Signalled"
	StatusDangerousSyscall = "Dangerous Syscall"
	StatusInternalError    = "Internal Error"
)

type Cmd struct {
	Args          []string         `json:"args"`
	Env           []string         `json:"env,omitempty"`
	Files         []*File          `json:"files,omitempty"` // [stdin, stdout, stderr]; nil = pipe
	CPULimit      uint64           `json:"cpuLimit"`
	ClockLimit    uint64           `json:"clockLimit"`
	MemoryLimit   uint64           `json:"memoryLimit"`
	StackLimit    uint64           `json:"stackLimit,omitempty"`
	ProcLimit     uint64           `json:"procLimit"`
	CopyIn        map[string]*File `json:"copyIn,omitempty"`
	CopyOut       []string         `json:"copyOut,omitempty"`
	CopyOutCached []string         `json:"copyOutCached,omitempty"`
	CopyOutMax    uint64           `json:"copyOutMax,omitempty"`
}

// File: exactly one group set.
type File struct {
	Src     string  `json:"src,omitempty"`     // host file (testdata, checker binary)
	Content *string `json:"content,omitempty"` // inline
	FileID  string  `json:"fileId,omitempty"`  // go-judge file store
	Name    string  `json:"name,omitempty"`    // collector (stdout / stderr)
	Max     uint64  `json:"max,omitempty"`     // collector cap
	// Pipe makes a collector truncate at Max instead of failing the write; go-judge still
	// reports CollectSizeExceeded in fileError (used for stderr).
	Pipe bool `json:"pipe,omitempty"`
}

// FileError is reported per file; Type "CollectSizeExceeded" marks a collector overflow
// (go-judge v1.13: stdout overflow appears here, not always as status OLE).
type FileError struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

const FileErrorCollectSize = "CollectSizeExceeded"

type PipeEnd struct {
	Index int `json:"index"`
	Fd    int `json:"fd"`
}

type PipeMap struct {
	In  PipeEnd `json:"in"`
	Out PipeEnd `json:"out"`
}

type Request struct {
	RequestID   string    `json:"requestId,omitempty"`
	Cmd         []Cmd     `json:"cmd"`
	PipeMapping []PipeMap `json:"pipeMapping,omitempty"`
}

type Result struct {
	Status     string            `json:"status"`
	Error      string            `json:"error,omitempty"`
	ExitStatus int               `json:"exitStatus"`
	Time       uint64            `json:"time"`    // ns, CPU
	RunTime    uint64            `json:"runTime"` // ns, wall
	Memory     uint64            `json:"memory"`  // bytes, peak
	Files      map[string]string `json:"files,omitempty"`
	FileIDs    map[string]string `json:"fileIds,omitempty"`
	FileError  []FileError       `json:"fileError,omitempty"`
}

// Overflowed reports whether the named collector exceeded its cap.
func (r Result) Overflowed(name string) bool {
	for _, fe := range r.FileError {
		if fe.Name == name && fe.Type == FileErrorCollectSize {
			return true
		}
	}
	return false
}

// Helpers for building requests.

func Str(s string) *string { return &s }

func Ms(v uint64) uint64                      { return v * uint64(time.Millisecond) }
func Sec(v uint64) uint64                     { return v * uint64(time.Second) }
func Dur(d time.Duration) uint64              { return uint64(d) }
func MiB(v uint64) uint64                     { return v << 20 }
func GiB(v uint64) uint64                     { return v << 30 }
func Collector(name string, max uint64) *File { return &File{Name: name, Max: max} }
func EmptyStdin() *File                       { return &File{Content: Str("")} }

// TruncatingCollector caps output without failing the program (contestant stderr).
func TruncatingCollector(name string, max uint64) *File {
	return &File{Name: name, Max: max, Pipe: true}
}
