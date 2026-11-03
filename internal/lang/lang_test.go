package lang

import (
	"reflect"
	"strings"
	"testing"
)

const reg = `
cpp20-gcc13:
  display: "C++20 (g++ 13)"
  source_name: main.cpp
  compile:
    args: ["/usr/bin/g++", "-O2", "-std=c++20", "-o", "a", "{sources}"]
    artifact: a
  run:
    args: ["./a"]
  env: ["PATH=/usr/bin:/bin"]
py312:
  source_name: main.py
  compile: { args: ["/usr/bin/python3.12", "-m", "py_compile", "main.py"], artifact: main.py }
  run: { args: ["/usr/bin/python3.12", "main.py"] }
`

func TestParseAndExpand(t *testing.T) {
	r, err := Parse([]byte(reg))
	if err != nil {
		t.Fatal(err)
	}
	l, _ := r.Get("cpp20-gcc13")
	got := l.CompileArgs([]string{"main.cpp", "harness.cpp"})
	want := []string{"/usr/bin/g++", "-O2", "-std=c++20", "-o", "a", "main.cpp", "harness.cpp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	got = l.CompileArgsExtra([]string{"c.cpp"}, "-I/usr/include/testlib")
	if got[1] != "-I/usr/include/testlib" || got[len(got)-1] != "c.cpp" {
		t.Fatal(got)
	}
	py, _ := r.Get("py312")
	if py.RunProcs != 1 || py.Ext() != ".py" {
		t.Fatal(py)
	}
}

func TestValidation(t *testing.T) {
	_, err := Parse([]byte("bad:\n  display: x\n"))
	if err == nil || !strings.Contains(err.Error(), "source_name") || !strings.Contains(err.Error(), "artifact") || !strings.Contains(err.Error(), "run.args") {
		t.Fatal(err)
	}
}
