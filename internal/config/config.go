// Package config loads and validates the worker's YAML configuration (§4).
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type StepLimits struct {
	CPU    Duration `yaml:"cpu"`
	Wall   Duration `yaml:"wall"`
	Memory ByteSize `yaml:"memory"`
	Procs  uint64   `yaml:"procs"`
	Output ByteSize `yaml:"output"`
}

type Config struct {
	WorkerID string `yaml:"worker_id"`
	Slots    int    `yaml:"slots"`

	GoJudge struct {
		URL                 string   `yaml:"url"`
		AuthTokenFile       string   `yaml:"auth_token_file"`
		RequestTimeoutExtra Duration `yaml:"request_timeout_extra"`
	} `yaml:"gojudge"`

	Redis struct {
		Addr           string   `yaml:"addr"`
		PasswordFile   string   `yaml:"password_file"`
		Group          string   `yaml:"group"`
		Lanes          []string `yaml:"lanes"`
		IdlePoll       Duration `yaml:"idle_poll"`
		Heartbeat      Duration `yaml:"heartbeat"`
		ReclaimMinIdle Duration `yaml:"reclaim_min_idle"`
		ReclaimEvery   Duration `yaml:"reclaim_every"`
	} `yaml:"redis"`

	API struct {
		EventsURL string `yaml:"events_url"`
		// JobsURL is the base for POST {jobs_url}/{job_id}/refresh (§6.1). Derived from
		// events_url ("…/internal/judge-jobs") when empty.
		JobsURL   string `yaml:"jobs_url"`
		TokenFile string `yaml:"token_file"`
		SpoolDir  string `yaml:"spool_dir"`
	} `yaml:"api"`

	Storage struct {
		Endpoint      string `yaml:"endpoint"`
		Bucket        string `yaml:"bucket"`
		Region        string `yaml:"region"`
		AccessKeyFile string `yaml:"access_key_file"`
		SecretKeyFile string `yaml:"secret_key_file"`
	} `yaml:"storage"`

	Cache struct {
		Dir      string   `yaml:"dir"`
		MaxBytes ByteSize `yaml:"max_bytes"`
	} `yaml:"cache"`

	Limits struct {
		Compile    StepLimits `yaml:"compile"`
		Checker    StepLimits `yaml:"checker"`
		StdoutMax  ByteSize   `yaml:"stdout_max"`
		StderrMax  ByteSize   `yaml:"stderr_max"`
		PreviewMax ByteSize   `yaml:"preview_max"`
	} `yaml:"limits"`

	ScratchDir string `yaml:"scratch_dir"`

	Health struct {
		// BaselineFile lives on a writable volume; the root filesystem is read-only.
		BaselineFile   string   `yaml:"baseline_file"`
		Interval       Duration `yaml:"interval"`
		TimingTol      float64  `yaml:"timing_tolerance"`
		TimingCheck    bool     `yaml:"timing_check"`
		NetworkProbeIP string   `yaml:"network_probe_ip"`
	} `yaml:"health"`

	// CAFile optionally adds an internal CA for API, storage and presigned URLs.
	CAFile string `yaml:"ca_file"`

	LanguagesFile string `yaml:"languages_file"`
	MetricsAddr   string `yaml:"metrics_addr"`
	LogLevel      string `yaml:"log_level"`

	// Loaded secrets (not from YAML).
	GoJudgeToken  string `yaml:"-"`
	RedisPassword string `yaml:"-"`
	APIToken      string `yaml:"-"`
	S3AccessKey   string `yaml:"-"`
	S3SecretKey   string `yaml:"-"`
}

func defaults() *Config {
	c := &Config{Slots: 4}
	c.GoJudge.URL = "http://127.0.0.1:5050"
	c.GoJudge.RequestTimeoutExtra = Duration(10 * time.Second)
	c.Redis.Group = "workers"
	c.Redis.Lanes = []string{"exam", "homework", "rejudge", "practice", "build"}
	c.Redis.IdlePoll = Duration(200 * time.Millisecond)
	c.Redis.Heartbeat = Duration(30 * time.Second)
	c.Redis.ReclaimMinIdle = Duration(10 * time.Minute)
	c.Redis.ReclaimEvery = Duration(time.Minute)
	c.API.SpoolDir = "/var/lib/judge-worker/spool"
	c.Storage.Bucket = "problems"
	c.Storage.Region = "us-east-1"
	c.Cache.Dir = "/var/lib/judge-worker/cache"
	c.Cache.MaxBytes = 50 << 30
	c.Limits.Compile = StepLimits{CPU: Duration(10 * time.Second), Wall: Duration(20 * time.Second), Memory: 1 << 30, Procs: 50, Output: 64 << 10}
	c.Limits.Checker = StepLimits{CPU: Duration(10 * time.Second), Wall: Duration(20 * time.Second), Memory: 1 << 30, Procs: 1, Output: 4 << 10}
	c.Limits.StdoutMax = 64 << 20
	c.Limits.StderrMax = 4 << 10
	c.Limits.PreviewMax = 64 << 10
	c.ScratchDir = "/var/lib/judge-worker/scratch"
	c.Health.BaselineFile = "/var/lib/judge-worker/state/baseline.json"
	c.Health.Interval = Duration(10 * time.Minute)
	c.Health.TimingTol = 0.10
	c.Health.TimingCheck = true
	c.Health.NetworkProbeIP = "1.1.1.1"
	c.LanguagesFile = "/etc/judge-worker/languages.yaml"
	c.MetricsAddr = "0.0.0.0:9100"
	c.LogLevel = "info"
	return c
}

// Load reads the config file, applies env overrides and reads every secret file.
// Any unreadable file is a startup error (§4).
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if v := os.Getenv("WORKER_ID"); v != "" {
		c.WorkerID = v
	}
	if c.API.JobsURL == "" && c.API.EventsURL != "" {
		c.API.JobsURL = strings.TrimSuffix(strings.TrimSuffix(c.API.EventsURL, "/"), "/judge-events") + "/judge-jobs"
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	secrets := []struct {
		file string
		dst  *string
		req  bool
	}{
		{c.GoJudge.AuthTokenFile, &c.GoJudgeToken, false},
		{c.Redis.PasswordFile, &c.RedisPassword, false},
		{c.API.TokenFile, &c.APIToken, true},
		{c.Storage.AccessKeyFile, &c.S3AccessKey, true},
		{c.Storage.SecretKeyFile, &c.S3SecretKey, true},
	}
	for _, s := range secrets {
		if s.file == "" {
			if s.req {
				return nil, errors.New("config: a required secret file path is empty")
			}
			continue
		}
		b, err := os.ReadFile(s.file)
		if err != nil {
			return nil, fmt.Errorf("read secret: %w", err)
		}
		*s.dst = strings.TrimSpace(string(b))
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []string
	if c.WorkerID == "" {
		errs = append(errs, "worker_id is empty (set it or WORKER_ID)")
	}
	if strings.ContainsAny(c.WorkerID, " /") {
		errs = append(errs, "worker_id must not contain spaces or '/'")
	}
	if c.Slots < 1 || c.Slots > 64 {
		errs = append(errs, "slots must be 1..64")
	}
	if c.Redis.Addr == "" {
		errs = append(errs, "redis.addr is empty")
	}
	if len(c.Redis.Lanes) == 0 {
		errs = append(errs, "redis.lanes is empty")
	}
	if c.API.EventsURL == "" {
		errs = append(errs, "api.events_url is empty")
	}
	if c.Storage.Endpoint == "" || c.Storage.Bucket == "" {
		errs = append(errs, "storage.endpoint and storage.bucket are required")
	}
	if c.Cache.Dir == "" || c.API.SpoolDir == "" || c.ScratchDir == "" {
		errs = append(errs, "cache.dir, api.spool_dir and scratch_dir are required")
	}
	if c.Redis.Heartbeat.D() <= 0 || c.Redis.ReclaimMinIdle.D() <= c.Redis.Heartbeat.D() {
		errs = append(errs, "redis.reclaim_min_idle must be greater than redis.heartbeat")
	}
	if len(errs) > 0 {
		return fmt.Errorf("config invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}
