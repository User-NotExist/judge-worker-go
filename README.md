# judge-worker

`judge-worker` runs untrusted programming-contest submissions safely and reports their results. Each judge host runs two containers:

| Container | Role | Runs as | Listens on |
|---|---|---|---|
| `go-judge` | Third-party sandbox ([criyle/go-judge](https://github.com/criyle/go-judge), MIT). It runs one program per request under CPU, wall-clock, memory and process limits. The C++ and Python toolchains live inside this image. | root, privileged (needs namespaces and cgroup v2) | `127.0.0.1:5050` inside its network namespace only |
| `judge-worker` | This project (Go). It pulls jobs from Redis, caches problem packages from S3 storage, runs every compile, run and check inside go-judge, scores the submission, and posts events to your API. | UID 65532, read-only root filesystem, no capabilities | `:9100` (metrics and health) |

Basic rules:

- Untrusted code never runs in the worker itself. Student programs, checkers and interactors all run inside go-judge.
- The worker keeps no state that needs backing up. The cache, spool and scratch directories can be deleted at any time; they are rebuilt.
- The worker never talks to your database. Its inputs come from Redis and S3, and its outputs go to one HTTP endpoint on your API.
- Results are deterministic: testcases run one after another, in a fixed order.
- Every event carries an idempotency key, so resending an event is always safe.

## Contents

1. [How a job flows](#how-a-job-flows)
2. [Requirements](#requirements)
3. [Installation](#installation): build the images, prepare a host
4. [Configuration](#configuration): `config.yaml`, `languages.yaml`, secrets
5. [Deployment](#deployment): bring a host up, upgrades, rollback, draining
6. [Endpoints and contracts](#endpoints-and-contracts), with examples
   - [Worker HTTP endpoints](#worker-http-endpoints-metrics-healthz) (`/metrics`, `/healthz`)
   - [Job queue (Redis)](#job-queue-redis-streams)
   - [Problem package format](#problem-package-format)
   - [Events API (your API receives these)](#events-api-implemented-by-your-api)
   - [Job refresh endpoint](#job-refresh-endpoint-implemented-by-your-api)
7. [Verdicts and scoring](#verdicts-and-scoring)
8. [Operations](#operations): CLI, signals, metrics, alerts, runbooks
9. [Local development](#local-development)
10. [Source layout](#source-layout)
11. [Behaviour notes and known limitations](#behaviour-notes-and-known-limitations)

---

## How a job flows

```
API ──XADD──▶ Redis stream judge:{lane}:{course}
                    │ XREADGROUP (strict lane priority, round-robin across courses)
                    ▼
             judge-worker slot ──download package (S3, cached by SHA-256)
                    │          ──download sources (presigned URLs, hash-checked)
                    │          ──compile / run / check ──▶ go-judge (127.0.0.1:5050)
                    │          ──score subtasks
                    ▼
       POST events ──▶ API /internal/judge-events   (spooled to disk if the API is down)
                    │
             XACK + XDEL once the API has accepted "done"
```

Each slot runs one job from start to finish. The number of slots equals go-judge's `-parallelism`, so a slot never waits for a free sandbox. While a job runs, the worker keeps its Redis message claimed. If a worker dies, another one reclaims the message and runs the job again under a higher attempt number. Your API rejects late events from the old attempt with `409`.

---

## Requirements

**Judge host (production)**

| Item | Requirement |
|---|---|
| OS / kernel | Linux ≥ 5.19 with cgroup v2 (Ubuntu 24.04 LTS, or Debian 12 with a backports kernel) |
| Docker | Docker Engine ≥ 24 with Compose v2 |
| Machine | Dedicated to judging, on an isolated network segment. Use identical hardware on every judge host so timings are comparable. |
| Swap | Off. Memory limits are wrong when swap is on. |
| Time | `chrony` (or another NTP client) running, offset < 50 ms |
| Size (default 4 slots) | 6 cores (2 for the worker and OS, 4 for judging), 16 GiB RAM, 100 GiB disk |

**Services the worker needs to reach**

| Service | Used for |
|---|---|
| Redis ≥ 6.2 (7.x recommended) | Job queue (Streams + consumer groups) |
| S3-compatible storage (RustFS, MinIO, …) | Problem packages. Use a **read-only** key scoped to the problems bucket. |
| Your API | The events endpoint and the job-refresh endpoint ([contracts below](#endpoints-and-contracts)) |

**To build:** Docker with BuildKit. Go 1.26 is needed only for running the unit tests locally.

---

## Installation

### 1. Build the images

Build both images in CI, scan them, push them to your registry, and deploy them **by digest** so every host runs exactly the same bits.

```sh
# Sandbox + toolchains (g++ 13, CPython 3.12, testlib). Pinned: go-judge v1.13.3, testlib f0169a8.
docker build -t registry.internal/judge/go-judge:1.13.3 images/go-judge
docker push registry.internal/judge/go-judge:1.13.3

# Worker (static binary on distroless, non-root). Build context is the repository root.
docker build -f images/judge-worker/Dockerfile \
  --build-arg VERSION="$(git describe --always --dirty)" \
  -t registry.internal/judge/judge-worker:1.0.0 .
docker push registry.internal/judge/judge-worker:1.0.0

# Digests to put into compose.yaml:
docker inspect --format '{{index .RepoDigests 0}}' registry.internal/judge/go-judge:1.13.3
docker inspect --format '{{index .RepoDigests 0}}' registry.internal/judge/judge-worker:1.0.0
```

To upgrade go-judge or testlib, change the build arguments in [images/go-judge/Dockerfile](images/go-judge/Dockerfile) (`GO_JUDGE_VERSION`, `GO_JUDGE_SHA256` from the release's `checksums.txt`, `TESTLIB_COMMIT`). Then run the self-test ([Operations](#operations)) before rolling the new image out.

The sandbox mounts are defined in [images/go-judge/mount.yaml](images/go-judge/mount.yaml): read-only binds of `/bin /lib /lib64 /usr`, and size-capped tmpfs at `/w` and `/tmp`. Never add `/var/lib/judge-worker` to it. That directory holds the testdata and expected answers.

### 2. Prepare the host

All host files are in [deploy/host/](deploy/host/). Apply them with your config management, then reboot.

| Setting | File on the host | Source in this repo |
|---|---|---|
| SMT off, THP `madvise` | append `nosmt transparent_hugepage=madvise` to `GRUB_CMDLINE_LINUX` in `/etc/default/grub`, then `update-grub` | [grub-cmdline.txt](deploy/host/grub-cmdline.txt) |
| ASLR off, swappiness 0 | `/etc/sysctl.d/90-judge.conf` | [90-judge.conf](deploy/host/90-judge.conf) |
| Host services on cores 0–1 | `/etc/systemd/system.conf`: `CPUAffinity=0 1` | — |
| IRQs off the judging cores | `/etc/default/irqbalance`: `IRQBALANCE_BANNED_CPULIST=2-5` | — |
| Governor `performance`, turbo off | `/usr/local/sbin/judge-tuning.sh` + `/etc/systemd/system/judge-tuning.service` | [judge-tuning.sh](deploy/host/judge-tuning.sh), [judge-tuning.service](deploy/host/judge-tuning.service) |
| Swap off | `swapoff -a` and remove swap from `/etc/fstab` | — |
| Docker daemon | `/etc/docker/daemon.json` | [daemon.json](deploy/host/daemon.json) |
| Firewall | `DOCKER-USER` iptables chain | [docker-user-firewall.sh](deploy/host/docker-user-firewall.sh) |
| Verification | `/usr/local/sbin/judge-check-host` | [judge-check-host](deploy/host/judge-check-host) |

```sh
sudo install -m 0755 deploy/host/judge-tuning.sh   /usr/local/sbin/
sudo install -m 0755 deploy/host/judge-check-host  /usr/local/sbin/
sudo install -m 0644 deploy/host/judge-tuning.service /etc/systemd/system/
sudo install -m 0644 deploy/host/90-judge.conf     /etc/sysctl.d/
sudo install -m 0644 deploy/host/daemon.json       /etc/docker/
sudo systemctl daemon-reload && sudo systemctl enable judge-tuning.service
sudo swapoff -a                       # and remove swap entries from /etc/fstab
sudo update-grub && sudo reboot       # after editing GRUB_CMDLINE_LINUX
```

Notes:

- Do **not** use `isolcpus` for the judging cores. Isolated cores drop out of load balancing, so parallel runs can pile onto one core.
- Docker writes its own iptables rules, and published ports bypass `ufw`/`firewalld`. Restrict access in the `DOCKER-USER` chain instead: allow only the management network to reach ports `9100` and `5052`, and allow outbound traffic only to the API, Redis, storage, NTP and the registry. Edit the IPs in `docker-user-firewall.sh` and persist the rules.
- `judge-check-host` checks SMT, ASLR, swap, the CPU governor, the go-judge cpuset and the container's swap limit. Run it after the containers are up; it exits non-zero and names the failing check.

---

## Configuration

Everything lives under `/opt/judge/` on the host. Start from [deploy/opt-judge/](deploy/opt-judge/):

```
/opt/judge/
├── compose.yaml               # both containers (image digests pinned here)
├── .env                       # WORKER_ID, MGMT_IP                 0644
├── config/                    # mounted read-only at /etc/judge-worker
│   ├── config.yaml
│   └── languages.yaml
└── secrets/                   # root:root 0700
    ├── go-judge.env           # ES_AUTH_TOKEN=<token>              0600
    └── worker/                # 0750, group 65532, mounted at /run/secrets/judge
        ├── gojudge.token      # same token as in go-judge.env      0640
        ├── api.token          # bearer token for your API          0640
        ├── redis.password                                          0640
        ├── s3.key             # read-only storage key              0640
        └── s3.secret                                               0640
```

### Secrets

Deliver secrets with your config management (Ansible Vault or similar). Never bake them into images or commit them. For example:

```sh
sudo install -d -m 0700 /opt/judge/secrets
sudo install -d -m 0750 -g 65532 /opt/judge/secrets/worker
TOKEN=$(openssl rand -hex 32)
echo "ES_AUTH_TOKEN=$TOKEN" | sudo tee /opt/judge/secrets/go-judge.env >/dev/null
echo "$TOKEN"               | sudo tee /opt/judge/secrets/worker/gojudge.token >/dev/null
sudo chmod 0600 /opt/judge/secrets/go-judge.env
sudo chmod 0640 /opt/judge/secrets/worker/* && sudo chgrp 65532 /opt/judge/secrets/worker/*
```

### `.env`

```sh
WORKER_ID=judge-01      # unique per host; becomes the Redis consumer name prefix and event worker_id
MGMT_IP=10.20.0.11      # address the metrics ports are published on
```

### `config.yaml`

The full example is in [deploy/opt-judge/config/config.yaml](deploy/opt-judge/config/config.yaml). The worker refuses to start on unknown keys, unreadable secret files, or an invalid language registry.

| Key | Default | Meaning |
|---|---|---|
| `worker_id` | — | Host identity. Overridden by the `WORKER_ID` environment variable. |
| `slots` | `4` | Concurrent jobs. **Must equal go-judge `-parallelism`.** |
| `gojudge.url` | `http://127.0.0.1:5050` | go-judge REST endpoint |
| `gojudge.auth_token_file` | — | Bearer token for go-judge |
| `gojudge.request_timeout_extra` | `10s` | Added to a run's wall-clock limit to get the HTTP timeout |
| `redis.addr` / `redis.password_file` | — | Redis connection |
| `redis.group` | `workers` | Consumer group (created automatically if missing) |
| `redis.lanes` | `[exam, homework, rejudge, practice, build]` | Lanes in strict priority order |
| `redis.idle_poll` | `200ms` | Sleep when every stream is empty |
| `redis.heartbeat` | `30s` | How often a running job re-claims its message |
| `redis.reclaim_min_idle` | `10m` | Idle time after which another worker takes over a message |
| `redis.reclaim_every` | `1m` | How often one slot scans for abandoned messages |
| `api.events_url` | — | Your events endpoint |
| `api.jobs_url` | derived from `events_url` (`…/judge-events` → `…/judge-jobs`) | Base URL for job refresh |
| `api.token_file` | — | Bearer token sent to your API |
| `api.spool_dir` | `/var/lib/judge-worker/spool` | Where events wait while the API is unreachable |
| `storage.endpoint` / `bucket` / `region` | — / `problems` / `us-east-1` | S3 storage (path-style addressing) |
| `storage.access_key_file` / `secret_key_file` | — | Read-only storage key |
| `cache.dir` / `cache.max_bytes` | `/var/lib/judge-worker/cache` / `50GiB` | Package cache. Least-recently-used packages are evicted above the limit. |
| `limits.compile` | `cpu 10s, wall 20s, memory 1GiB, procs 50, output 64KiB` | Limits for compiling submissions, checkers and interactors |
| `limits.checker` | `cpu 10s, wall 20s, memory 1GiB, procs 1, output 4KiB` | Limits for testlib checkers |
| `limits.stdout_max` / `stderr_max` / `preview_max` | `64MiB` / `4KiB` / `64KiB` | Contestant output caps. `preview_max` caps the output preview for sample testcases. |
| `scratch_dir` | `/var/lib/judge-worker/scratch` | Per-job temporary files |
| `health.interval` | `10m` | How often each slot re-runs the self-test |
| `health.timing_check` / `timing_tolerance` | `true` / `0.10` | Compare a CPU benchmark with the host baseline (±10 %) |
| `health.baseline_file` | `/var/lib/judge-worker/state/baseline.json` | Created by the first successful benchmark or by `selftest` |
| `health.network_probe_ip` | `1.1.1.1` | Address sandboxed programs must fail to reach |
| `ca_file` | — | Extra CA bundle for the API, storage and presigned URLs |
| `languages_file` | `/etc/judge-worker/languages.yaml` | Language registry |
| `metrics_addr` | `0.0.0.0:9100` | Metrics and health listener |
| `log_level` | `info` | `debug`, `info`, `warn` or `error` |

Durations use Go syntax (`200ms`, `10s`, `10m`). Sizes accept `B`, `KiB`, `MiB`, `GiB`, `TiB`, `KB`, `MB` and `GB`.

### `languages.yaml`

Languages are configuration, not code. See [deploy/opt-judge/config/languages.yaml](deploy/opt-judge/config/languages.yaml).

```yaml
cpp20-gcc13:                       # language id used in jobs and problem.yaml
  display: "C++20 (g++ 13)"
  source_name: main.cpp            # name the primary source file is saved as
  compile:
    args: ["/usr/bin/g++", "-O2", "-std=c++20", "-pipe", "-static", "-DONLINE_JUDGE",
           "-o", "a", "{sources}"] # {sources} = every source file name, in job order
    artifact: a                    # file kept after compiling
  run:
    args: ["./a"]
  run_procs: 1                     # process/thread limit for the contestant
  env: ["PATH=/usr/bin:/bin"]
  version_args: ["/usr/bin/g++", "--version"]   # optional; first output line is reported as "toolchain"
```

- `cpp20-gcc13` is **required**: checkers and interactors are compiled with it, plus `-I/usr/include/testlib`.
- For interpreted languages the artifact is the source file itself (`artifact: main.py`). Every other source file is copied next to it at run time, so a harness can import the student's module.
- Time limits are **not** set per language here. They come with each job, already resolved for that job's language.
- To add a language: install the toolchain in the go-judge image, add an entry here, roll out the image, and run the self-test.

---

## Deployment

### Bring up a new judge host

1. Install the OS and apply the [host preparation](#2-prepare-the-host). Reboot.
2. Install Docker Engine and `daemon.json`, then apply the `DOCKER-USER` firewall rules.
3. Place `/opt/judge/`: `compose.yaml` with the image digests, `.env` with a **unique** `WORKER_ID`, `config/`, and `secrets/`.
4. Start the containers:
   ```sh
   cd /opt/judge
   docker compose pull
   docker compose up -d
   docker compose ps                     # judge-worker should become "healthy" within a few minutes
   ```
5. Run the full hostile suite and timing test. This also writes the host's timing baseline:
   ```sh
   docker compose exec judge-worker judge-worker selftest
   sudo judge-check-host
   ```
6. Confirm the host appears in monitoring with healthy slots. It starts pulling jobs on its own; there is no registration step.

What [compose.yaml](deploy/opt-judge/compose.yaml) sets up:

- go-judge is privileged and pinned to cores 2–5. It has a 6 GiB memory limit with `memswap_limit` set to the same value (no swap), and listens on `127.0.0.1:5050` only. Port 5050 is never published.
- The worker shares go-judge's network namespace (`network_mode: service:go-judge`), so `127.0.0.1:5050` reaches the sandbox. Because of that, the worker's port `9100` is published on the go-judge service, bound to `MGMT_IP`. go-judge's own metrics are published on `MGMT_IP:5052`.
- The worker runs as UID 65532 with a read-only root filesystem, `cap_drop: ALL` and `no-new-privileges`. It is pinned to cores 0–1.
- The volume `judge-cache` is mounted at the **same path** in both containers (read-only in go-judge). The worker passes testdata to the sandbox as host file paths; the files are copied into each run and never mounted into it.
- The worker also gets three volumes: `judge-spool` (unsent events), `judge-scratch` (per-job temporary files) and `judge-state` (timing baseline).
- `stop_grace_period: 330s` lets running jobs finish when the worker is stopped.

### Test profile (staging or a shared machine)

```sh
docker compose -f compose.yaml -f compose.override.test.yaml up -d
```

This replaces pinned cores with CPU shares and keeps swap disabled. Expect noisy timings: set `health.timing_check: false` and use generous time limits. **Never** judge real submissions or run `selftest` (the hostile suite) on a shared machine. A privileged container is effectively root on that host.

### Drain, upgrade, roll back

| Task | Command / procedure |
|---|---|
| Drain a host | `docker compose stop judge-worker`. Waits up to 330 s for running jobs; anything unfinished is reclaimed by other hosts. |
| Upgrade | One host at a time. Use the first host as a canary for a day. Update the digests in `compose.yaml`, then `docker compose pull && docker compose up -d`, then `selftest`. |
| Verify | Re-judge reference solutions and compare their times with the previous baseline. More than 10 % drift should block the rollout. |
| Roll back | Restore the previous digests, then `docker compose up -d`. Keep the last three digests in the registry. |
| Rotate the go-judge token | Change both `go-judge.env` and `worker/gojudge.token`, then `docker compose up -d` on a drained host. |
| Rotate the API, Redis or storage secrets | Have the server accept the old and new value for an overlap period, update the secret file, and restart the hosts one by one. |
| Pre-load a package (exam day) | `docker compose exec judge-worker judge-worker warm -package-sha <sha256> -package-key packages/<sha256>.zip` |

Never upgrade during an exam window.

### State and recovery

Judge hosts hold nothing that needs backing up:

| Data | If lost |
|---|---|
| `judge-cache` | Re-downloaded on demand |
| `judge-spool` | Matters only while the API is down. Un-acked jobs are re-run when reclaimed. |
| `judge-scratch` | Nothing |
| `judge-state` | The baseline is recreated by the next `selftest` |
| `/opt/judge` | Re-applied by config management |

To replace a failed host, rebuild it rather than repairing it. Its in-flight jobs are reclaimed automatically.

---

## Endpoints and contracts

### Worker HTTP endpoints (`/metrics`, `/healthz`)

Served on `metrics_addr` (default `:9100`). On a production host this is reachable at `http://$MGMT_IP:9100` from the management network only.

**`GET /healthz`** returns `200` when at least one slot is healthy, and `503` when every slot is quarantined.

```sh
$ curl -i http://10.20.0.11:9100/healthz
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8

ok
```
```
HTTP/1.1 503 Service Unavailable

no healthy slot
```

The worker image has no shell or curl. Docker's healthcheck runs `judge-worker healthcheck` instead, which calls this endpoint and exits 0 or 1.

**`GET /metrics`** returns metrics in the Prometheus text format.

```sh
$ curl -s http://10.20.0.11:9100/metrics | grep '^judge_'
judge_jobs_total{kind="judge",lane="exam",verdict="AC"} 42
judge_job_duration_seconds_bucket{kind="judge",lane="exam",le="2"} 39
judge_queue_wait_seconds_bucket{lane="exam",le="5"} 41
judge_testcase_duration_seconds_count{language="cpp20-gcc13"} 380
judge_gojudge_requests_total{status="Accepted"} 512
judge_gojudge_errors_total{kind="transport"} 0
judge_events_spooled 0
judge_cache_bytes 1.2843e+09
judge_worker_slot_healthy{slot="0"} 1
judge_worker_busy_slots 2
judge_events_dropped_total 0
judge_worker_alerts_total{reason="checker_failed"} 0
```

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `judge_jobs_total` | counter | `lane`, `kind`, `verdict` | Finished jobs. For builds, `verdict` is `ok` or the failure reason. |
| `judge_job_duration_seconds` | histogram | `lane`, `kind` | Job start to `done` |
| `judge_queue_wait_seconds` | histogram | `lane` | Job `created_at` to start |
| `judge_testcase_duration_seconds` | histogram | `language` | Run + check per testcase |
| `judge_gojudge_requests_total` | counter | `status` | go-judge results by status string |
| `judge_gojudge_errors_total` | counter | `kind` (`transport`, `internal`, `file`) | Sandbox failures |
| `judge_events_spooled` | gauge | — | Events waiting on disk for the API |
| `judge_cache_bytes` | gauge | — | Package cache size |
| `judge_worker_slot_healthy` | gauge | `slot` | 1 if the slot passed its last health check |
| `judge_worker_busy_slots` | gauge | — | Slots currently running a job |
| `judge_events_dropped_total` | counter | — | Events your API rejected with a 4xx other than 409/429 (a bug on one side) |
| `judge_worker_alerts_total` | counter | `reason` | Conditions that need an operator, such as `unknown_status`, `poison`, `bad_job`, `invalid_package`, `checker_compile_failed`, `checker_failed`, `manager_failed`, `slot_unhealthy`, `slot_quarantined` or `disk_full` |

go-judge exposes its own metrics on `:5052` (`-enable-metrics`). Scrape it too.

### Job queue (Redis Streams)

Your API enqueues jobs, and the worker consumes them.

| Key | Type | Written by | Contents |
|---|---|---|---|
| `judge:{lane}:{course_id}` | stream | API (`XADD`) | One entry per job, field `data` = job JSON |
| `judge:build:_` | stream | API | Build jobs |
| `judge:streams:{lane}` | set | API (`SADD`) | Every stream name in the lane that may hold jobs |

- The worker never uses `KEYS` or `SCAN`. A stream is visible only once its name is in `judge:streams:{lane}`, so add it in the **same transaction** as the `XADD`. `SADD` is idempotent. A cleanup task on the API side may remove streams that have been empty for an hour.
- Consumer group: `workers`. The worker creates it with `XGROUP CREATE <stream> workers 0` on first contact, so jobs added before the group existed are not lost. Consumer names are `{worker_id}/slot-{n}`, plus `{worker_id}/spool` for messages whose `done` event is still waiting in the spool.
- Priority: lanes are served in strict `redis.lanes` order. Within a lane, the worker rotates round-robin across course streams, which gives each course a fair share.
- Settling a message: `XACK` then `XDEL` once your API has accepted the `done` (or `build_done`) event.

Enqueue example:

```sh
redis-cli <<'EOF'
MULTI
XADD judge:exam:12 * data '{"kind":"judge","job_id":"01JC9Z3T7Q2Y7M4W8X0K3F5B6A","submission_id":48213,"attempt":1,"course_id":12,"lane":"exam","language":"cpp20-gcc13","source":{"files":[{"name":"main.cpp","url":"https://rustfs.internal/problems/sources/9f2c...?X-Amz-Signature=...","sha256":"9f2c...64 hex..."}]},"problem":{"id":301,"version":4,"package_sha256":"41ab...64 hex...","package_key":"packages/41ab....zip"},"limits":{"time_ms":1000,"memory_mb":256},"feedback":{"preview_samples":true},"created_at":"2026-11-03T11:50:00Z"}'
SADD judge:streams:exam judge:exam:12
EXEC
EOF
```

#### JudgeJob

```json
{
  "kind": "judge",
  "job_id": "01JC9Z3T7Q2Y7M4W8X0K3F5B6A",
  "submission_id": 48213,
  "attempt": 1,
  "course_id": 12,
  "lane": "exam",
  "language": "cpp20-gcc13",
  "source": {
    "files": [
      { "name": "main.cpp",    "url": "https://rustfs.internal/…presigned GET…", "sha256": "9f2c…" },
      { "name": "harness.cpp", "url": "https://rustfs.internal/…presigned GET…", "sha256": "77aa…" }
    ]
  },
  "problem": { "id": 301, "version": 4, "package_sha256": "41ab…", "package_key": "packages/41ab….zip" },
  "limits": { "time_ms": 1000, "memory_mb": 256 },
  "feedback": { "preview_samples": true },
  "created_at": "2026-11-03T11:50:00Z"
}
```

| Field | Rules |
|---|---|
| `job_id` | Required. Used in event ids and the refresh URL. |
| `submission_id`, `attempt` | Required, ≥ 1. If the message was delivered more than once, the worker reports `attempt + deliveries − 1`. |
| `language` | A registry id, or `"output-only"` for output-only problems |
| `source.files` | Final source set, already assembled (fill-in-the-blank code, grader stubs, harnesses). The first file is the primary source. Names: `[A-Za-z0-9._-]`, max 128 characters, unique. `sha256` is 64 lowercase hex characters and is verified after download. Max 16 MiB per file (64 MiB for output-only files). |
| `problem.package_sha256` | SHA-256 of the package zip. The package is cached under this hash. |
| `limits.time_ms` | CPU limit for **this language**, 1–60000. The wall-clock limit is `2 × time_ms + 1000` ms. |
| `limits.memory_mb` | Memory limit, 1–8192. The stack limit is the same. |
| `feedback.preview_samples` | If true, the output of `sample: true` testcases is included in events. Output of hidden testcases never leaves the host. |
| Output-only | `language: "output-only"`; `source.files` holds one file per testcase, named `<testcase>.out`. |

Presigned URLs should be valid for 30 minutes. When the worker reclaims a job whose `created_at` is more than 25 minutes old, it asks your API for fresh URLs first ([refresh endpoint](#job-refresh-endpoint-implemented-by-your-api)).

#### BuildJob (generate expected outputs)

A build job runs a reference solution on a staging package and uploads the outputs as `.ans` files. It is used when a problem is uploaded.

```json
{
  "kind": "build",
  "job_id": "01JC9ZB3…",
  "problem": { "id": 301, "version": 5, "package_sha256": "aa10…", "package_key": "staging/aa10….zip" },
  "reference": {
    "language": "cpp20-gcc13",
    "files": [ { "name": "main.cpp", "url": "…presigned GET…", "sha256": "…" } ]
  },
  "limits": { "time_ms": 1000, "memory_mb": 256 },
  "answers_upload": {
    "1.ans": "https://rustfs.internal/…presigned PUT…",
    "2.ans": "https://rustfs.internal/…presigned PUT…"
  }
}
```

The worker's storage key is read-only. Answers are written **only** through the presigned PUT URLs, one per testcase. Build jobs support `batch` problems, and the staging package may omit `.ans` files.

### Problem package format

A zip file, stored in the bucket under `package_key` and identified by its SHA-256:

```
problem.yaml
tests/1.in   tests/1.ans   tests/2.in   tests/2.ans …
checker/checker.cpp        # only for checker.kind: testlib (testlib.h is provided by the image)
manager/manager.cpp        # only for type: interactive
```

```yaml
type: batch                    # batch | interactive | output_only
limits:
  memory_mb: 256
  time_ms:                     # one entry for every allowed language
    cpp20-gcc13: 1000
    py312: 3000
checker:
  kind: default                # default | testlib
  float_abs: 1e-6              # default checker only; omit both for exact token compare
  float_rel: 1e-6
  strict: false                # true = also report presentation errors (PE); see "Strict mode"
  # points_max: 100            # testlib only: "points X" is divided by this (default 1)
testcases:                     # declaration order = execution order
  - { name: "1", sample: true }
  - { name: "2" }
  - { name: "3" }
  - { name: "4" }
subtasks:
  - { index: 0, points: 0,  mode: min, testcases: ["1"] }          # samples, worth nothing
  - { index: 1, points: 30, mode: min, testcases: ["2", "3"] }
  - { index: 2, points: 70, mode: min, testcases: ["2", "3", "4"], depends_on: [1] }
```

A package is rejected, and the job ends `SE` with a reason, if any of these fail:

- unsafe zip entries: absolute paths, `..`, symlinks, or files larger than 1 GiB
- unknown `type` or `checker.kind`
- missing `tests/<name>.in` (not required for output-only problems)
- missing `tests/<name>.ans` (not required for interactive problems or build jobs)
- no `time_ms` entry for the job's language
- a subtask that names an unknown testcase, has negative or non-integer points, an invalid mode, or a `depends_on` pointing at a later subtask
- a missing checker or manager source
- a checker or manager that doesn't compile (`checker_compile_failed` / `manager_compile_failed`)

Packages that fail permanently are remembered, so later jobs fail fast without downloading the package again.

**Checker contract (testlib):** the checker is run as `checker input output answer`. It is interpreted as follows:

| Checker result | Fraction | Verdict |
|---|---|---|
| exit 0 | 1 | AC |
| exit 0 with a number in [0, 1] on stdout | that number | AC / PA / WA |
| exit 1 (WA) | 0 | WA |
| exit 2 (PE) | 0 | WA, or **PE** when `checker.strict: true` |
| exit 7 with stderr `points X …` (testlib `quitp`) | X / `points_max`, clamped to [0, 1] | AC / PA / WA |
| exit 3 (`_fail`), any other exit, crash or timeout | — | **JE** |

The message shown to students is the checker's stderr, truncated to 1 KiB.

**Interactor contract:** the manager is run as `manager input`, where `input` is `tests/<name>.in`. Its stdin and stdout are connected to the contestant's stdout and stdin. It must write a fraction between `0.0` and `1.0` on the first line of `score.txt` and exit 0. Its stderr is the message. Any other exit, or a missing or invalid score, is a judge error (`JE`). If the contestant crashes or times out, the contestant's verdict wins. The exception is when the manager had already decided WA and the contestant then died of SIGPIPE: that reports WA.

### Events API (implemented by your API)

```
POST {api.events_url}
Authorization: Bearer <api token>
Content-Type: application/json
```

The body is one event object. When replaying the spool, the worker sends a **JSON array** of up to 50 events. Your API must accept both forms.

How the worker treats your response:

| Response | Worker behaviour |
|---|---|
| `2xx` | Accepted. Return `2xx` for duplicates too: `event_id` is the idempotency key. |
| `409 Conflict` | This attempt is superseded (a newer attempt exists). The worker stops the job and does **not** ack the message. |
| `429`, `5xx`, network error | Retried 3 times (exponential backoff with jitter, 0.5 s up to 30 s). After that the event goes to the on-disk spool and the job keeps running. Later events of the same job also go to the spool, so the order is preserved. |
| any other `4xx` | Treated as a bug: logged, dropped, counted in `judge_events_dropped_total`. |

The Redis message is acked only after `done` / `build_done` has been accepted, even when it arrives through the spool. The replayer keeps the message claimed while it waits.

#### Envelope

```json
{
  "event_id": "48213:2:7",
  "seq": 7,
  "type": "test_result",
  "submission_id": 48213,
  "attempt": 2,
  "job_id": "01JC9Z3T7Q2Y7M4W8X0K3F5B6A",
  "worker_id": "judge-01",
  "sent_at": "2026-11-03T11:50:04.120Z",
  "payload": { }
}
```

- `event_id` = `{submission_id}:{attempt}:{seq}` for judge jobs, and `build-{job_id}:{attempt}:{seq}` for build jobs (which have no `submission_id`).
- `seq` starts at 1 for each attempt and increases by one per event.
- Event types arrive in this order: `started`, `compiled`, `test_result` / `skipped` (repeated), then `done`. A build job sends a single `build_done`.

#### Example: one submission, end to end

```sh
curl -X POST https://api.internal/internal/judge-events \
  -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" \
  -d '{"event_id":"48213:1:1","seq":1,"type":"started","submission_id":48213,"attempt":1,
       "job_id":"01JC9Z3T7Q2Y7M4W8X0K3F5B6A","worker_id":"judge-01",
       "sent_at":"2026-11-03T11:50:01.002Z","payload":{"slot":1,"go_judge_version":"v1.13.3"}}'
```

Payloads of the events that followed (this was a wrong-answer submission):

```jsonc
// seq 1 — started
{ "slot": 1, "go_judge_version": "v1.13.3" }

// seq 2 — compiled   (ok:false → followed directly by done{verdict:"CE"})
{ "ok": true, "time_ms": 93, "log": "", "truncated": false }

// seq 3 — test_result for a sample testcase: output_preview is present
{ "testcase": "1", "subtasks": [0], "verdict": "WA", "time_ms": 1, "memory_kb": 284,
  "fraction": 0, "checker_msg": "token 1: expected 3, found -1", "output_preview": "-1\n" }

// seq 4 — test_result for a hidden testcase: no output_preview
{ "testcase": "2", "subtasks": [1, 2], "verdict": "WA", "time_ms": 1, "memory_kb": 284,
  "fraction": 0, "checker_msg": "token 1: expected 300, found -100" }

// seq 5, 6 — skipped (early stop; dependency not met)
{ "subtask": 1, "testcases": ["3"] }
{ "subtask": 2, "testcases": ["3", "4"] }

// seq 7 — done
{ "verdict": "WA", "score_milli": 0, "subtask_scores_milli": [0, 0, 0], "reason": null,
  "go_judge_version": "v1.13.3", "toolchain": "g++ (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0",
  "duration_ms": 171 }
```

| Type | Payload fields |
|---|---|
| `started` | `slot`, `go_judge_version` |
| `compiled` | `ok`, `time_ms`, `log` (compiler stdout + stderr, ≤ 64 KiB), `truncated` |
| `test_result` | `testcase`; `subtasks` (indexes containing it); `verdict`; `time_ms` (CPU); `memory_kb` (peak); `fraction` (0–1); `checker_msg` (≤ 1 KiB); `output_preview` and `preview_truncated` (sample testcases only, when allowed) |
| `skipped` | `subtask`, `testcases` that will not run for that subtask (a later subtask may still run them) |
| `done` | `verdict`; `score_milli` (integer milli-points, `null` for SE/JE); `subtask_scores_milli`; `reason` (`null` or a reason code); `detail` (optional text); `go_judge_version`; `toolchain`; `duration_ms` |
| `build_done` | `ok`; `answers` (`[{name, sha256, bytes}]`); `max_time_ms` (slowest testcase of the reference solution, to help set time limits); `reason`, `testcase`, `log`, `detail` (on failure); `problem_id`, `version` |

Successful build example:

```json
{ "ok": true, "max_time_ms": 1, "problem_id": 301, "version": 5,
  "answers": [ { "name": "1.ans", "sha256": "1121cfcc…02a2", "bytes": 2 },
               { "name": "2.ans", "sha256": "f807fe6d…6468", "bytes": 4 } ] }
```

Failed build example:

```json
{ "ok": false, "reason": "reference_tle", "testcase": "4", "max_time_ms": 1003, "problem_id": 301, "version": 5 }
```

**`done.reason` codes** (`verdict` is `SE` unless noted):

| Reason | Meaning |
|---|---|
| `judge_error` (verdict `JE`) | A checker or interactor failed. Hold the submission for staff review instead of scoring it. |
| `bad_job` | The job JSON is malformed or invalid, or doesn't match the problem type |
| `poison` | The message was delivered more than 3 times |
| `unknown_language` | The language is not in the registry |
| `package_hash_mismatch` | The downloaded package doesn't match `package_sha256` |
| `invalid_package` | `problem.yaml` or the package contents failed validation |
| `checker_compile_failed`, `manager_compile_failed` | The package's checker or interactor doesn't compile |
| `source_download_failed`, `source_hash_mismatch` | A presigned source URL was rejected (for example, expired), or its hash didn't match |
| `sandbox_internal_error`, `sandbox_file_error`, `sandbox_error` | go-judge kept failing after one retry |
| `unknown_status` | go-judge returned a status this version doesn't know |

Build-only reasons: `reference_ce`, `reference_<verdict>` (for example `reference_tle` or `reference_re`, with `testcase` set), `missing_upload_url`, `answer_upload_failed`, `unsupported_problem_type`.

Some failures produce **no** `done` event: go-judge unreachable, storage unreachable, a failed refresh, or disk full. The message stays un-acked and another worker reclaims it later as a new attempt. The 3-delivery poison limit stops endless retries.

### Job refresh endpoint (implemented by your API)

Called only for reclaimed jobs whose `created_at` is more than 25 minutes old, because their presigned URLs may have expired.

```
POST {api.jobs_url}/{job_id}/refresh
Authorization: Bearer <api token>
Content-Type: application/json

{}
```

```sh
curl -X POST https://api.internal/internal/judge-jobs/01JC9Z3T7Q2Y7M4W8X0K3F5B6A/refresh \
  -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" -d '{}'
```

| Response | Worker behaviour |
|---|---|
| `200` with the job JSON (same schema as in Redis, with fresh URLs), or `{"job": {…}}` | Runs the refreshed job. The attempt number is the higher of the worker's computed attempt and the returned one. |
| `404` / `410` | The job no longer exists: ack and drop it |
| anything else, or a network error | Leaves the message un-acked; it is retried later |

### go-judge (dependency)

The worker uses go-judge's `POST /run`, `GET /file/{id}`, `POST /file`, `DELETE /file/{id}`, `GET /version` and `GET /config`. Calls carry `Authorization: Bearer <token>`. go-judge must listen on localhost only: in v1.13.3, `/version` and `/config` don't require the token (`/run` and `/file` do).

---

## Verdicts and scoring

| Verdict | Meaning |
|---|---|
| `AC` | Accepted |
| `PA` | Partially accepted (fraction strictly between 0 and 1) |
| `WA` | Wrong answer, or a missing output-only file |
| `PE` | Presentation error: the content is right but the layout is wrong. Only produced when the problem sets `checker.strict: true` ([Strict mode](#strict-mode-presentation-errors)). Score 0. |
| `TLE` | CPU or wall-clock limit exceeded (message `cpu 1.02 s` / `wall 3.00 s`) |
| `MLE` | Memory limit exceeded (`peak 257 MiB`) |
| `OLE` | stdout larger than `stdout_max` |
| `RE` | Nonzero exit (`exit code 3`), signal (`signal 11 (SIGSEGV)`), or a blocked system call |
| `CE` | Compilation failed (score 0) |
| `SE` | System error (see the reason codes) |
| `JE` | Judge error: a checker or interactor failed (no score) |

- **Default checker:** compares whitespace-separated tokens byte for byte. If `float_abs` / `float_rel` are set, two finite numbers also match when `|a−b| ≤ float_abs` or `|a−b| ≤ float_rel·|b|`. Extra or missing tokens are WA. Example message: `token 17: expected 42, found 41`. By default whitespace is ignored: `1 2 3`, `1\n2\n3` and `  1 2 3  \r\n\n` all match the answer `1 2 3`. To check spacing too, turn on [strict mode](#strict-mode-presentation-errors).
- **Contestant stderr** is capped at `stderr_max` and truncated; overflowing stderr does not fail a run.
- **Subtask scoring:**
  - Subtasks are evaluated in index order. Testcases run in `problem.yaml` declaration order, and a testcase shared by several subtasks runs only once per job.
  - In `min` mode, a subtask is worth `points × min(fractions)`. It stops at the first testcase with fraction 0; the remaining testcases are reported as `skipped`.
  - In `sum` mode, a subtask is worth `points × mean(fractions)`, with no early stop.
  - A subtask whose `depends_on` subtasks did not score full points gets 0, and its testcases are skipped.
  - Scores are integer milli-points, rounded down per subtask. `done.verdict` is the first non-AC testcase verdict in execution order, otherwise AC.

### Strict mode (presentation errors)

Set `checker.strict: true` in `problem.yaml` to enable the `PE` verdict for that problem. Without it, problems behave exactly as described above and `PE` is never reported.

```yaml
checker:
  kind: default
  strict: true
```

**With the default checker**, tokens are compared first, exactly as in normal mode, and float tolerances still apply. Only when **every token matches** is the whitespace layout compared with the answer file:

- every separator between tokens must be identical (`" "` is not `"  "`, `"\t"` or `"\n"`)
- no whitespace before the first token unless the answer has it
- the end of the output must match the answer's trailing whitespace

Two differences are always tolerated:

- `\r\n` counts as `\n` (Windows line endings)
- if the answer ends with no newline or exactly one, the output may also end with either

A wrong token is still reported as `WA` even if the layout is also wrong. The answer files define the exact expected layout, so make sure they don't contain stray trailing spaces.

| Output (answer is `1 2\n3\n`) | Normal | Strict | Strict message |
|---|---|---|---|
| `1 2\n3\n` | AC | AC | |
| `1 2\n3` (no final newline) | AC | AC | |
| `1 2\r\n3\r\n` | AC | AC | |
| `1  2\n3\n` | AC | **PE** | `presentation error before token 2: expected " ", found "  "` |
| `1 2 3\n` | AC | **PE** | `presentation error before token 3: expected "\n", found " "` |
| ` 1 2\n3\n` | AC | **PE** | `presentation error at start of output: expected nothing, found " "` |
| `1 2\n3 \n` | AC | **PE** | `presentation error at end of output: expected "\n", found " \n"` |
| `1 2\n3\n\n` | AC | **PE** | `presentation error at end of output: expected "\n", found "\n\n"` |
| `1 2\n4\n` | WA | WA | `token 3: expected 3, found 4` |

Long whitespace runs are summarised in the message: the first 32 characters, then `… (100000 whitespace chars)`.

**With a testlib checker**, the checker decides what counts as a formatting error, typically through testlib's strict reading (`readSpace()`, `readEoln()`, `readEof()`) or an explicit `quitf(_pe, …)`. In strict mode, exit code 2 is reported as `PE` with the checker's message. Without strict mode it stays `WA`.

Strict mode doesn't apply to interactive problems: there the interactor alone decides the result.

**For API and UI integrators:** `PE` can appear as a `test_result.verdict` and as the `done.verdict` (the first non-AC testcase in execution order). It scores 0, like `WA`. Your API should accept it as a verdict value.

---

## Operations

### CLI

```sh
judge-worker [-config /etc/judge-worker/config.yaml]          # run (default command)
judge-worker healthcheck                                       # exit 0 if /healthz is OK (Docker healthcheck)
judge-worker selftest [-skip-timing]                           # hostile suite + timing test; writes the baseline
judge-worker warm -package-sha <sha256> -package-key <key>     # pre-load a package into the cache
judge-worker version
```

Run them inside the container: `docker compose exec judge-worker judge-worker <command>`. The subcommand comes first, then its flags.

`selftest` prints a JSON report and exits 1 on any failure. It runs:

- A+B in C++ and Python
- a fork bomb, an infinite loop, `sleep(1000)`, allocating and touching 2 GiB, 1 GiB of stdout, deep recursion
- reading `/etc/shadow`, searching the filesystem for `*.ans`, connecting to an external IP, filling `/w`, spawning threads past the process limit, `exit(3)`
- compile bombs (`#include "/dev/random"`, template recursion, a flood of compiler errors)
- the timing test: a CPU benchmark run 20 times on every slot in parallel; each slot's spread must be below 2 %

Afterwards the host must still be healthy. Run `selftest` after every go-judge, kernel, Docker or toolchain change, and only on dedicated judge hosts.

### Health checks

At startup, every `health.interval` per slot, and on `SIGUSR1`, each slot runs a quick self-test:

- go-judge `/version` and `/config` respond
- A+B gives AC in each language
- an infinite loop gives TLE; allocating 2 GiB under a 256 MiB limit gives MLE
- network access fails
- the cache directory is invisible inside the sandbox
- if `timing_check` is on, the benchmark is within the tolerance of the baseline

A slot that fails is **quarantined**: it stops taking jobs and `judge_worker_slot_healthy{slot}` drops to 0. A slot is also quarantined after two `SE` results in a row. A quarantined slot retries its health check every minute and re-enables itself when the check passes.

### Signals

| Signal | Effect |
|---|---|
| `SIGTERM` / `SIGINT` (first) | Stop pulling, let running jobs finish (up to 5 min), flush the spool, exit 0 |
| second `SIGTERM` | Exit immediately. Unfinished jobs are reclaimed by other workers. |
| `SIGUSR1` | Run the health check on every slot |

### Logs

JSON lines on stdout (collected by Docker's `json-file` driver; ship them to your log store). Every job line carries `worker_id`, `slot`, `job_id`, `submission_id`, `attempt`, `course_id` and `lane`. Source code, testdata, program output and tokens are never logged.

```json
{"time":"2026-11-03T13:48:24.89Z","level":"INFO","msg":"job started","worker_id":"judge-01","slot":0,"stream":"judge:exam:1","msg_id":"1793713657843-0","job_id":"01JC9Z…","deliveries":2,"submission_id":48213,"attempt":2,"course_id":1,"lane":"exam","kind":"judge","problem_id":301,"problem_version":4,"language":"py312","reclaimed":true}
```

### Suggested alerts

| Alert | Condition |
|---|---|
| Exam backlog | `histogram_quantile(0.95, judge_queue_wait_seconds{lane="exam"}) > 60` for 2 min |
| Judge error | `increase(judge_jobs_total{verdict="JE"}[5m]) > 0` |
| System errors | SE above 1 % of jobs over 10 min |
| Spool not draining | `judge_events_spooled > 0` for 5 min |
| Slot unhealthy | `judge_worker_slot_healthy == 0` for 5 min |
| Host missing | no scrape for 2 min |
| Operator attention | `increase(judge_worker_alerts_total[10m]) > 0` |

### Runbooks

- **Many `SE` on one host:** quarantine the host with `docker compose stop judge-worker`. Check `docker compose logs go-judge`, the free disk space, and `dmesg` for OOM or cgroup errors. Run `selftest` before returning the host to service.
- **Many `JE` on one problem:** its checker or interactor is broken. Fix the package, publish a new version, and rejudge the held submissions.
- **Spool not draining:** the events endpoint is down or rejecting the token. Check the API's health and look for `events API HTTP …` in the worker logs. Jobs are not lost; they stay un-acked until replay succeeds.
- **Queue growing while slots are idle:** check the worker logs for Redis errors, and check that the stream is listed in `judge:streams:{lane}`.
- **Host down:** the other hosts reclaim its jobs after `reclaim_min_idle`. If capacity is short during an exam, pause the practice and rejudge lanes on the API side.

---

## Local development

```sh
go test ./...                                              # unit tests, no Docker needed
docker compose -f deploy/dev/compose.yaml up -d --build    # Redis, RustFS, devapi, go-judge, worker
examples/smoke.sh                                          # 23 end-to-end cases (AC/WA/PE/CE/RE/TLE/MLE/OLE, strict mode, testlib, interactive, output-only, function tasks)
docker compose -f deploy/dev/compose.yaml down -v          # stop and delete volumes
```

The dev stack ([deploy/dev/](deploy/dev/)) uses the test profile:

- 2 slots, no pinned cores, timing check off
- reclaim after 40 s instead of 10 min, so a crash test finishes quickly
- RustFS credentials `devkey` / `devsecret123`; dev tokens in `deploy/dev/secrets/worker/`

Two dev-only tools stand in for the API:

**`devtool`** packages a problem directory, uploads it and the sources, and enqueues the job exactly as the API would:

```sh
DC="docker compose -f deploy/dev/compose.yaml --profile tools"
$DC run --rm devtool submit -id 1 -lane exam -problem /examples/problems/aplusb \
    -lang cpp20-gcc13 -files /examples/solutions/aplusb/ac.cpp
$DC run --rm devtool wait -id 1          # prints the done payload
$DC run --rm devtool build -problem /examples/problems/aplusb-staging \
    -lang cpp20-gcc13 -files /examples/solutions/aplusb/ac.cpp
# Function task: name each file explicitly
$DC run --rm devtool submit -problem /examples/problems/function -lang py312 \
    -files /examples/solutions/function/harness.py,/examples/solutions/function/solution.py -names main.py,solution.py
```

**`devapi`** is an in-memory events endpoint on `http://127.0.0.1:18080`. It enforces the attempt rule and returns `409` for an older attempt.

| Endpoint | Purpose |
|---|---|
| `POST /internal/judge-events` | Receives events (object or array); token `dev-api-token` |
| `POST /internal/judge-jobs/{id}/refresh` | Always `404` |
| `GET /events[?submission_id=N]` | Stored events as a JSON array |
| `POST /admin/down?for=45s` | Answer `503` for a while, to test the spool |

```sh
curl -s 'localhost:18080/events?submission_id=1' | jq -c '.[] | [.seq, .type]'
curl -s -X POST 'localhost:18080/admin/down?for=45s'      # simulate an API outage
curl -s localhost:19100/metrics | grep ^judge_             # worker metrics in the dev stack
```

Example problems are in [examples/problems/](examples/problems/):

- `aplusb`: batch, default checker, subtasks
- `aplusb-strict`: the same problem with `checker.strict: true`
- `aplusb-staging`: no `.ans` files, for build jobs
- `partial`: testlib checker with partial points
- `interactive`: guess the number
- `outputonly`
- `function`: harness + student file

Matching solutions are in [examples/solutions/](examples/solutions/).

---

## Source layout

```
cmd/judge-worker/       entry point, slot loop, heartbeat/reclaim handling, shutdown
cmd/devapi, cmd/devtool development stand-ins for the API
internal/config         config.yaml loading and validation
internal/lang           language registry
internal/job            job types and validation
internal/queue          Redis Streams consumer: lanes, fairness, heartbeat, reclaim, ack
internal/gojudge        go-judge REST client
internal/pkgcache       package download, verification, safe unzip, cache + LRU eviction, problem.yaml
internal/pipeline       batch / interactive / output-only / build pipelines, checker & interactor compile
internal/checker        default token checker (incl. strict layout mode), testlib result parsing
internal/verdict        go-judge status → verdict
internal/score          subtask scoring
internal/report         events client, spool, replay
internal/health         self-test and hostile suite
internal/metrics        Prometheus collectors
images/                 Dockerfiles and the sandbox mount config
deploy/opt-judge/       production /opt/judge template
deploy/host/            host tuning, firewall and verification scripts
deploy/dev/             local development stack
examples/               sample problems, solutions, smoke.sh
```

---

## Behaviour notes and known limitations

- **Attempt numbering:** the job JSON in Redis never changes. A redelivered job runs as `attempt + deliveries − 1`, so your API always sees a strictly higher attempt from the new worker.
- **Infrastructure failures don't produce `SE`.** If go-judge or storage is unreachable, no `done` is sent. The worker stops pulling until go-judge has answered `/version` for 60 s straight, and the job is reclaimed later.
- **Output limit detection:** go-judge 1.13 reports stdout overflow as a file error (`CollectSizeExceeded`), often with a nonzero-exit status. The worker maps that to `OLE`.
- **Parallelism can't be verified:** go-judge 1.13.3 doesn't expose its `-parallelism` in `/config`. The worker logs a warning at startup, so keep `slots` and `-parallelism` equal in your config management.
- **Checker and interactor binaries** are fetched with `GET /file`, because go-judge's inline `copyOut` returns file contents as JSON strings, which isn't safe for binaries.
- **Cache volume ownership:** the go-judge image pre-creates `/var/lib/judge-worker/cache` owned by UID 65532. A new named volume is seeded by the first container that mounts it, which is go-judge, so without this the worker couldn't write to it.
- **Compile results are not cached** between jobs: a rejudge always recompiles.
- **Timing consistency** depends on the host tuning and identical hardware. On untuned machines, keep `timing_check` off and use generous limits.
