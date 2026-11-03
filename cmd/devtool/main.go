// Command devtool plays the API's role for local testing: it packages a problem
// directory, uploads it and the sources to S3, and enqueues judge or build jobs in Redis
// exactly as the API would (XADD + SADD judge:streams:{lane}). Development only.
//
//	devtool submit -problem /examples/problems/aplusb -lang cpp20-gcc13 -files /examples/solutions/aplusb/ac.cpp [-id 1] [-lane exam] [-course 1]
//	devtool build  -problem /examples/problems/aplusb -lang cpp20-gcc13 -files /examples/solutions/aplusb/ac.cpp
//	devtool wait   -id 1 [-timeout 120s]        print the done event for a submission
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "devtool: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: devtool submit|build|wait …")
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	problem := fl.String("problem", "", "problem directory")
	langID := fl.String("lang", "cpp20-gcc13", "language id or output-only")
	files := fl.String("files", "", "comma-separated source files; the first is saved as the language's source name unless -names is given")
	names := fl.String("names", "", "comma-separated names for -files (default: main.<ext> for the first, basename for the rest)")
	id := fl.Int64("id", time.Now().Unix()%1_000_000, "submission id")
	attempt := fl.Int("attempt", 1, "attempt")
	lane := fl.String("lane", "homework", "lane")
	course := fl.String("course", "1", "course id")
	preview := fl.Bool("preview", true, "feedback.preview_samples")
	timeout := fl.Duration("timeout", 2*time.Minute, "wait: timeout")
	_ = fl.Parse(os.Args[2:])

	switch cmd {
	case "wait":
		wait(*id, *timeout)
		return
	case "submit", "build":
	default:
		die("unknown command %s", cmd)
	}
	if *problem == "" {
		die("-problem is required")
	}
	ctx := context.Background()
	s3, bucket := s3Client()
	ensureBucket(ctx, s3, bucket)

	zipBytes := zipDir(*problem)
	sum := sha256.Sum256(zipBytes)
	pkgSHA := hex.EncodeToString(sum[:])
	prefix := "packages/"
	if cmd == "build" {
		prefix = "staging/"
	}
	pkgKey := prefix + pkgSHA + ".zip"
	put(ctx, s3, bucket, pkgKey, zipBytes)

	var py struct {
		Limits struct {
			MemoryMb uint64            `yaml:"memory_mb"`
			TimeMs   map[string]uint64 `yaml:"time_ms"`
		} `yaml:"limits"`
		Testcases []struct {
			Name string `yaml:"name"`
		} `yaml:"testcases"`
	}
	b, err := os.ReadFile(filepath.Join(*problem, "problem.yaml"))
	if err != nil {
		die("%v", err)
	}
	if err := yaml.Unmarshal(b, &py); err != nil {
		die("%v", err)
	}
	tl := py.Limits.TimeMs[*langID]
	if tl == 0 {
		tl = 1000
	}

	type fileRef struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
	}
	var refs []fileRef
	var nameList []string
	if *names != "" {
		nameList = strings.Split(*names, ",")
	}
	for i, f := range splitNonEmpty(*files) {
		data, err := os.ReadFile(f)
		if err != nil {
			die("%v", err)
		}
		s := sha256.Sum256(data)
		h := hex.EncodeToString(s[:])
		key := "sources/" + h
		put(ctx, s3, bucket, key, data)
		u, err := s3.PresignedGetObject(ctx, bucket, key, 30*time.Minute, url.Values{})
		if err != nil {
			die("presign: %v", err)
		}
		name := filepath.Base(f)
		switch {
		case i < len(nameList):
			name = nameList[i]
		case i == 0 && *langID != "output-only":
			name = "main" + filepath.Ext(f)
		}
		refs = append(refs, fileRef{Name: name, URL: u.String(), SHA256: h})
	}
	jobID := ulidish()
	job := map[string]any{
		"job_id":     jobID,
		"problem":    map[string]any{"id": 1, "version": 1, "package_sha256": pkgSHA, "package_key": pkgKey},
		"limits":     map[string]any{"time_ms": tl, "memory_mb": max(py.Limits.MemoryMb, 16)},
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	stream := fmt.Sprintf("judge:%s:%s", *lane, *course)
	if cmd == "submit" {
		job["kind"] = "judge"
		job["submission_id"] = *id
		job["attempt"] = *attempt
		job["course_id"] = 1
		job["lane"] = *lane
		job["language"] = *langID
		job["source"] = map[string]any{"files": refs}
		job["feedback"] = map[string]any{"preview_samples": *preview}
	} else {
		*lane = "build"
		stream = "judge:build:_"
		job["kind"] = "build"
		job["lane"] = "build"
		job["reference"] = map[string]any{"language": *langID, "files": refs}
		ups := map[string]string{}
		for _, tc := range py.Testcases {
			u, err := s3.PresignedPutObject(ctx, bucket, "answers/"+pkgSHA+"/"+tc.Name+".ans", 30*time.Minute)
			if err != nil {
				die("presign put: %v", err)
			}
			ups[tc.Name+".ans"] = u.String()
		}
		job["answers_upload"] = ups
	}
	data, _ := json.Marshal(job)
	rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")})
	_, err = rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{"data": string(data)}})
		p.SAdd(ctx, "judge:streams:"+*lane, stream)
		return nil
	})
	if err != nil {
		die("enqueue: %v", err)
	}
	fmt.Printf("enqueued %s job %s submission %d on %s (package %s…)\n", cmd, jobID, *id, stream, pkgSHA[:12])
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ulidish() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%013X%s", time.Now().UnixMilli(), strings.ToUpper(hex.EncodeToString(b))[:13])
}

func s3Client() (*minio.Client, string) {
	ep := env("S3_ENDPOINT", "rustfs:9000")
	cl, err := minio.New(ep, &minio.Options{
		Creds:        credentials.NewStaticV4(env("S3_KEY", "devkey"), env("S3_SECRET", "devsecret123"), ""),
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		die("s3: %v", err)
	}
	return cl, env("S3_BUCKET", "problems")
}

func ensureBucket(ctx context.Context, c *minio.Client, b string) {
	ok, err := c.BucketExists(ctx, b)
	if err != nil {
		die("s3: %v", err)
	}
	if !ok {
		if err := c.MakeBucket(ctx, b, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			die("make bucket: %v", err)
		}
	}
}

func put(ctx context.Context, c *minio.Client, bucket, key string, data []byte) {
	if _, err := c.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{}); err != nil {
		die("put %s: %v", key, err)
	}
}

// zipDir builds a deterministic zip (sorted names, fixed times) so the hash is stable.
func zipDir(dir string) []byte {
	var paths []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		rel, _ := filepath.Rel(dir, p)
		h := &zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate, Modified: time.Unix(0, 0).UTC()}
		h.SetMode(0o644)
		w, _ := zw.CreateHeader(h)
		data, _ := os.ReadFile(p)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func wait(id int64, timeout time.Duration) {
	api := env("DEVAPI_URL", "http://devapi:8080")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("%s/events?submission_id=%d", api, id))
		if err == nil {
			var evs []struct {
				Type    string          `json:"type"`
				Attempt int             `json:"attempt"`
				Payload json.RawMessage `json:"payload"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&evs)
			resp.Body.Close()
			for _, e := range evs {
				if e.Type == "done" {
					fmt.Println(string(e.Payload))
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	die("timeout waiting for submission %d", id)
}
