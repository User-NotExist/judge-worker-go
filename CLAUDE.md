# CLAUDE.md

Guidance for AI coding agents working in this repository. [README.md](README.md) is the user-facing reference (install, deploy, every contract, with examples); this file covers what you need in order to *change* the code safely.

## What this is

`judge-worker` is a Go service that judges programming-contest submissions. It pulls jobs from Redis Streams, caches problem packages from S3 (RustFS), runs every compile, run and check inside **go-judge** (a separate privileged sandbox container reached over `127.0.0.1:5050`), scores subtasks, and POSTs events to an external API. The API, scoreboard and UI live elsewhere. Module path: `judge-worker`.

## Commands

```sh
go build ./... && go vet ./...
go test -race -count=1 ./...                               # unit tests; no Docker needed
gofmt -l .                                                 # must print nothing

docker compose -f deploy/dev/compose.yaml up -d --build    # dev stack: redis, rustfs, devapi, go-judge, worker
examples/smoke.sh                                          # end-to-end: every case must print "ok"
curl -s localhost:18080/events | jq .                      # events the worker posted (devapi)
curl -s localhost:19100/metrics | grep ^judge_             # worker metrics in the dev stack
docker logs judge-dev-judge-worker-1                       # JSON logs
docker compose -f deploy/dev/compose.yaml down -v          # tear down, delete volumes
```

- `go.mod` requires Go 1.26. With an older local Go, `GOTOOLCHAIN=auto` (the default) downloads it.
- After changing Go code, rebuild before running the smoke test: `docker compose -f deploy/dev/compose.yaml up -d --build judge-worker`, then wait for `docker inspect -f '{{.State.Health.Status}}' judge-dev-judge-worker-1` to report `healthy` (the startup self-test takes about 5–30 s).
- The user's shell is **zsh** with aliases:
  - quote globs, e.g. `find . -name '*.go'`, or zsh fails with "no matches found"
  - `grep` is wrapped by a shell function that rejects `--include`: use `command grep -rn --include='*.go' …` or `rg -t go …`
  - `ls` is aliased to `eza` and has hung in non-interactive commands: use `find` or `command ls`

## Definition of done for a change

1. `gofmt -l .` is empty, and `go vet ./...` and `go test -race -count=1 ./...` pass.
2. New behaviour has unit tests in the package's `_test.go` (table tests are the norm here).
3. Anything that touches judging (`pipeline`, `checker`, `verdict`, `score`, `pkgcache`, `gojudge`) is verified end to end: rebuild the worker, run `examples/smoke.sh`, and add a smoke case plus an example problem or solution under `examples/` when the change adds a new behaviour.
4. Logs contain no unexpected `WARN`/`ERROR` lines. The one known warning is `go-judge /config does not expose parallelism`.
5. [README.md](README.md) is updated in the same change for every config key, `problem.yaml` field, verdict, event field, reason code, metric, CLI flag or endpoint you add or change. The README must stay self-contained: don't refer to the design document or its § numbers.

## Layout

```
cmd/judge-worker/   main.go (subcommands, wiring, signals), worker.go (slot loop, heartbeat,
                    reclaim, poison/bad-job handling, settle = ack or defer to spool, refresh)
cmd/devapi          dev-only fake events API (in memory; /events, /admin/down)
cmd/devtool         dev-only stand-in for the API: zips a problem, uploads, XADD+SADD
internal/config     YAML config + secret files; unknown keys are rejected (KnownFields)
internal/lang       languages.yaml registry; {sources} template expansion
internal/job        JudgeJob/BuildJob JSON + validation
internal/queue      Redis Streams: lane priority, round-robin, XAUTOCLAIM reclaim, XCLAIM heartbeat
internal/gojudge    typed REST client; retries transport errors/5xx once → ErrUnavailable
internal/pkgcache   package download, SHA-256 check, safe unzip, flock-protected cache, LRU eviction,
                    problem.yaml parsing + validation, checker/manager compile hook
internal/pipeline   per-job orchestration: batch, interactive, output-only, build; Exec for self-tests
internal/checker    default token checker (+ strict/PE mode), testlib exit-code interpretation
internal/verdict    go-judge Result → verdict
internal/score      subtask scoring (min/sum, early stop, depends_on, milli-points)
internal/report     event client, Reporter (seq per attempt), disk spool, Replayer
internal/health     quick self-test, hostile suite, timing baseline
internal/metrics    Prometheus collectors
images/             go-judge image (toolchains + pinned go-judge/testlib) and worker image
deploy/opt-judge/   production /opt/judge template; deploy/host/ host tuning; deploy/dev/ dev stack
examples/           sample problems, solutions, smoke.sh
```

Request flow: `worker.handle` → `Deps.RunJudge`/`RunBuild` (`pipeline/judge.go`, `build.go`) → `jobCtx` steps (`steps.go`, `interactive.go`) → `score.Score` with a per-testcase runner → `Deps.finish` emits `done` → `settle` acks the message, or defers the ack to the spool.

## Invariants: do not break these

- **Untrusted code runs only in go-judge.** The worker never executes student code, checkers or interactors itself. The in-process default checker is trusted code that only reads files.
- **Testdata never enters the sandbox filesystem.** Inputs and answers are passed as `copyIn` `src` paths, which go-judge copies into each run. Never add `/var/lib/judge-worker` to `images/go-judge/mount.yaml` or to a `copyIn` directory.
- **Hidden-case output never leaves the host.** `output_preview` only for `sample: true` testcases when `feedback.preview_samples` is true (`jobCtx.previewAllowed`).
- **Never log** source code, testdata, program output or tokens; log sizes, hashes and ids.
- **Determinism:** testcases run sequentially, in `problem.yaml` declaration order, and each one at most once per job.
- **Event contract:** `event_id` = `{submission_id}:{attempt}:{seq}` (builds: `build-{job_id}:{attempt}:{seq}`). `seq` is strictly increasing per attempt. Exactly one `done`/`build_done`, and it comes last. Additive payload changes are fine; renaming or removing fields breaks the external API. Document every change in the README.
- **Ack only after the API accepted `done`.** `XACK`+`XDEL` happen in `settle` or in the spool replayer, never earlier. On `409` (superseded) never ack: another worker owns the message.
- **Infrastructure failures are not verdicts.** If go-judge or storage is unreachable (`transientError`), send no `done` and don't ack. The message is reclaimed later as `attempt + deliveries − 1`, and the poison limit (>3 deliveries) caps retries. Don't turn these into `SE`.
- **Once a job has spooled one event, all its later events go to the spool too** (`Reporter.spooling`), so they are delivered in order.
- **Scores are integer milli-points**, floored per subtask. Fractions are float64 only per testcase.
- **The worker container is unprivileged:** UID 65532, read-only root filesystem, no shell (distroless). Anything written at runtime must go to a mounted volume (cache, spool, scratch, state) or `/tmp`.

## Gotchas learned the hard way

- **Never name a Go package directory `testdata`.** `go test ./...` silently skips such directories. This is why the package cache is `internal/pkgcache`; its tests went unrun for a while under the old name.
- **yaml.v3 silently truncates floats into integer fields** (`2.5` → `2`). Decode numbers that must be integers as `float64` and validate them (see `Subtask.Points`).
- **go-judge v1.13.3 specifics (pinned in `images/go-judge/Dockerfile`):**
  - stdout overflow shows up as `fileError[].type == "CollectSizeExceeded"`, often with status *Nonzero Exit Status*, not *Output Limit Exceeded*. `verdict.FromRun` checks `Result.Overflowed("stdout")` first.
  - Contestant stderr uses `TruncatingCollector` (`pipe: true`). An overflow there still sets status OLE plus a stderr `fileError`; `FromRun` treats that as a normal completion.
  - `/config` does not expose `parallelism`, so the startup check only warns.
  - `/version` and `/config` are unauthenticated; `/run` and `/file` require the bearer token.
  - `copyOut` returns file contents as JSON strings and is not binary-safe. Fetch binaries with `copyOutCached` + `GET /file/{id}` (see `HelperCompiler`).
  - Re-check these whenever go-judge is upgraded, then run the smoke test.
- **Named volumes are seeded by the first container that mounts them.** go-judge starts first, so its image pre-creates `/var/lib/judge-worker/cache` owned by 65532.
- **Dockerfiles don't allow trailing comments on `FROM`.**
- **The `minio/minio` image can't be pulled any more.** The dev stack uses `rustfs/rustfs:1.0.2`, and the S3 client is minio-go with path-style addressing.
- **miniredis stream idle times follow `SetTime`, not `FastForward`** (see `advance()` in `queue_test.go`).
- **go-redis `XReadGroupArgs.Block`:** `0` means block forever. Use `-1` for a non-blocking read.
- **`flock` locks:** jobs hold a shared lock on `.lock/<sha>` for the whole job, `Ensure` upgrades to exclusive while building, and eviction uses `LOCK_EX|LOCK_NB` so it never deletes a package that is in use. Each lock needs its own file descriptor, which is what makes it work across goroutines.

## Safety rules for agents

- **Never run the hostile suite (`judge-worker selftest`) on the dev machine.** The dev stack runs a privileged container on the user's shared machine. `selftest` is only for dedicated judge hosts.
- The startup and periodic quick self-test (A+B, TLE, MLE, network blocked, cache hidden) is safe and runs automatically.
- `deploy/dev/secrets/worker/*` are intentional dummy credentials needed by the dev stack. Never add real secrets anywhere; `.gitignore` blocks the usual secret file patterns.
- Changing `deploy/opt-judge/` or `deploy/host/` affects production hosts: keep them consistent with the README and call out such changes explicitly.
- Don't commit, push, or bump pinned image versions or digests unless asked.

## Testing patterns

- **Checker and scoring logic:** pure functions with table tests (`checker_test.go`, `score_test.go`, `verdict_test.go`).
- **Cache:** fake fetcher and fake compiler with temp dirs (`pkgcache_test.go`).
- **Queue:** miniredis (`queue_test.go`).
- **Events and spool:** `httptest` fake API (`report_test.go`).
- **New problem types or verdict paths:** add an example under `examples/problems/<name>/` and `examples/solutions/<name>/`, then add a `check` line to `examples/smoke.sh` with the expected verdict and `score_milli`.
- **Resilience scenarios not covered by `smoke.sh`:** run these by hand when touching `queue`, `report` or `worker.go`.
  - API outage: `curl -X POST 'localhost:18080/admin/down?for=45s'` before submitting. Expect `judge_events_spooled > 0`, then replay in order, then the stream emptied.
  - Worker crash: `docker kill -s KILL judge-dev-judge-worker-1` mid-job, then `up -d`. Expect attempt 2 and exactly one `done`. The dev config reclaims after 40 s.
  - Graceful stop: `docker compose stop judge-worker` mid-job. Expect the job to finish and exit code 0.

## Open items

- Worker status reporting: there is no push heartbeat or `/status` endpoint yet. The worker only exposes `/healthz` and `/metrics`. A Redis heartbeat key (`judge:workers:{id}` with a TTL) has been proposed but not built.
- The full hostile suite and the 20-run timing test have not been run on a real judge host yet.
