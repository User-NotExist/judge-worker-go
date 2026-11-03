package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestByteSize(t *testing.T) {
	for in, want := range map[string]uint64{"64KiB": 64 << 10, "1GiB": 1 << 30, "50GiB": 50 << 30, "512": 512, "4 MiB": 4 << 20} {
		got, err := ParseByteSize(in)
		if err != nil || uint64(got) != want {
			t.Fatalf("%s: %d %v", in, got, err)
		}
	}
	if _, err := ParseByteSize("lots"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	sec := func(n string) string {
		p := filepath.Join(dir, n)
		_ = os.WriteFile(p, []byte(" secret-"+n+"\n"), 0o600)
		return p
	}
	cfg := `worker_id: judge-01
slots: 2
redis: { addr: "r:6379", heartbeat: 30s, reclaim_min_idle: 10m }
api: { events_url: "https://api.internal/internal/judge-events", token_file: "` + sec("api") + `" }
storage: { endpoint: "https://s3:9000", access_key_file: "` + sec("k") + `", secret_key_file: "` + sec("s") + `" }
cache: { max_bytes: 10GiB }
limits:
  compile: { cpu: 5s, wall: 10s, memory: 512MiB, procs: 10, output: 32KiB }
`
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(cfg), 0o600)
	t.Setenv("WORKER_ID", "judge-07")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerID != "judge-07" || c.APIToken != "secret-api" || c.Cache.MaxBytes != 10<<30 {
		t.Fatalf("%+v", c)
	}
	if c.Limits.Compile.CPU.D() != 5*time.Second || c.Limits.Checker.Procs != 1 || c.Limits.StdoutMax != 64<<20 {
		t.Fatalf("%+v", c.Limits)
	}
	if c.API.JobsURL != "https://api.internal/internal/judge-jobs" {
		t.Fatal(c.API.JobsURL)
	}
	_ = os.WriteFile(p, []byte(cfg+"bogus_key: 1\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "bogus_key") {
		t.Fatal("unknown keys must fail", err)
	}
}
