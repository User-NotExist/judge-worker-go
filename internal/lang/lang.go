// Package lang holds the language registry (§5): compile and run command templates.
package lang

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Language struct {
	ID         string `yaml:"-"`
	Display    string `yaml:"display"`
	SourceName string `yaml:"source_name"`
	Compile    struct {
		Args     []string `yaml:"args"`
		Artifact string   `yaml:"artifact"`
	} `yaml:"compile"`
	Run struct {
		Args []string `yaml:"args"`
	} `yaml:"run"`
	RunProcs uint64   `yaml:"run_procs"`
	Env      []string `yaml:"env"`
	// VersionArgs (optional) is run once in go-judge at startup; the first output line is
	// reported as `toolchain` in done events, e.g. ["/usr/bin/g++", "--version"].
	VersionArgs []string `yaml:"version_args"`
}

// CompileArgs expands {sources} into every source file name, in job order.
func (l *Language) CompileArgs(sources []string) []string {
	return expand(l.Compile.Args, sources)
}

// CompileArgsExtra is CompileArgs with extra flags inserted right after argv[0]
// (used for checkers: -I/usr/include/testlib).
func (l *Language) CompileArgsExtra(sources []string, extra ...string) []string {
	args := l.CompileArgs(sources)
	if len(args) == 0 {
		return args
	}
	out := append([]string{args[0]}, extra...)
	return append(out, args[1:]...)
}

func expand(tmpl []string, sources []string) []string {
	out := make([]string, 0, len(tmpl)+len(sources))
	for _, a := range tmpl {
		if a == "{sources}" {
			out = append(out, sources...)
			continue
		}
		out = append(out, strings.ReplaceAll(a, "{sources}", strings.Join(sources, " ")))
	}
	return out
}

// Ext returns the primary source file extension, e.g. ".cpp".
func (l *Language) Ext() string { return path.Ext(l.SourceName) }

type Registry struct {
	langs map[string]*Language
}

func (r *Registry) Get(id string) (*Language, bool) {
	l, ok := r.langs[id]
	return l, ok
}

func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.langs))
	for id := range r.langs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func Load(file string) (*Registry, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read languages: %w", err)
	}
	return Parse(b)
}

func Parse(b []byte) (*Registry, error) {
	var m map[string]*Language
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse languages: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("languages: registry is empty")
	}
	var errs []string
	for id, l := range m {
		if l == nil {
			errs = append(errs, id+": empty entry")
			continue
		}
		l.ID = id
		if id == "output-only" {
			errs = append(errs, "output-only is reserved")
		}
		if l.SourceName == "" {
			errs = append(errs, id+": source_name missing")
		}
		if l.Compile.Artifact == "" {
			errs = append(errs, id+": compile.artifact missing")
		}
		if len(l.Compile.Args) == 0 {
			errs = append(errs, id+": compile.args missing")
		}
		if len(l.Run.Args) == 0 {
			errs = append(errs, id+": run.args missing")
		}
		if l.RunProcs == 0 {
			l.RunProcs = 1
		}
		for _, n := range []string{l.SourceName, l.Compile.Artifact} {
			if n != "" && (strings.Contains(n, "/") || n == "." || n == "..") {
				errs = append(errs, id+": file names must be plain names")
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("languages invalid: %s", strings.Join(errs, "; "))
	}
	return &Registry{langs: m}, nil
}
