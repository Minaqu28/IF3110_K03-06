# DBMP — Database Migration Platform

## Stress Testing (`k6`)

### Overview & Architecture

The stress test suite evaluates the platform's HTTP API responsiveness, per-database migration locking, RabbitMQ job dispatch, and async worker execution under concurrent load. It consists of three components:

1. **Migration Pool Seeder (`k6/seed/main.go`)**
   - Connects directly to the platform PostgreSQL database and ensures the bootstrap admin user belongs to a `k6-testers` group with `VIEW_EDIT` access and group leader status on all target connections.
   - Ensures a lightweight `public.k6_stress_log` table exists on both target databases (`shop` and `analytics`).
   - Seeds `K6_MIGRATION_POOL` (default `150`) pre-approved, reversible `APPLY` migrations in `NORMAL` execution mode per database (`300` total) and writes their IDs (`database_id`, `migration_id`, `revision_id`, `approval_request_id`) to `k6/migrations.json`.
2. **k6 Load & Migration Script (`k6/stress.js`)**
   - Maintains per-VU authenticated sessions (`noCookiesReset: true`) using the platform's CSRF login flow (`/api/v1/auth/login-csrf`, `/api/v1/auth/login`, `/api/v1/auth/session`) and automatically re-authenticates if a `401` is ever encountered.
   - Runs **three concurrent ramping-VU scenarios** over ~2 minutes (`125s` total stages + graceful ramp-down) and exports metrics to `k6/summary.json`.
3. **Runner Script (`scripts/k6.sh`)**
   - Loads `.env`, runs `k6/seed` inside a `golang:1.26-alpine` container attached to the Docker Compose network, and executes `grafana/k6:latest run /work/k6/stress.js`.

---

### Test Scenarios

| Scenario | Executor | VU Ramp Profile | Workload & Purpose |
| :--- | :--- | :--- | :--- |
| **`api_read_access`** | `ramping-vus` (`gracefulRampDown: 10s`) | `2` → `10` (20s) → `25` (30s) → `45` (30s) → `10` (30s) → `0` (15s) | Exercises authenticated read & query endpoints: `GET /api/v1/databases`, `GET /api/v1/databases/{id}`, `GET /api/v1/databases/{id}/history`, and `POST /api/v1/databases/{id}/query` (`SELECT 1 AS ok`), with `0.3s` think time per iteration. |
| **`migration_same_db`** | `ramping-vus` (`gracefulRampDown: 15s`) | `1` → `2` (20s) → `5` (30s) → `8` (30s) → `2` (30s) → `0` (15s) | Submits pre-approved migrations targeting **`shop`** via `POST /api/v1/databases/{id}/executions` (`202 Accepted`) and polls `GET /api/v1/jobs/{id}` every `250 ms` (`POLL_INTERVAL_SEC = 0.25`, timeout `60s`). Tests per-database lock contention (`409 Conflict` with `0.5s` backoff) and serialized queue execution. |
| **`migration_diff_db`** | `ramping-vus` (`gracefulRampDown: 15s`) | `1` → `2` (20s) → `4` (30s) → `6` (30s) → `2` (30s) → `0` (15s) | Alternates pre-approved migrations between **`analytics`** and **`shop`** and polls until terminal status (`SUCCEEDED`, `FAILED`, `CANCELLED`). Tests parallel async worker throughput across independent databases. |

**Peak Concurrency:** `45` read VUs + `8` same-DB migration VUs + `6` diff-DB migration VUs = **59 concurrent VUs**.

### Pass Thresholds

Configured in `k6/stress.js`:
- `http_req_duration`: `p(95) < 1500 ms`
- `http_req_failed`: `rate < 0.05` (`< 5%`)
- `api_response_time_ms`: `p(95) < 1500 ms`
- `api_error_rate`: `rate < 0.05` (`< 5%`)
- `job_execution_success_rate`: `rate > 0.95` (`> 95%`)

---

### How to Run

1. Start the Docker Compose stack and seed the initial platform data:
   ```sh
   docker compose up -d --build
   docker compose run --rm --entrypoint /seed app
   ```
2. Run the stress test:
   ```sh
   ./scripts/k6.sh
   ```
3. Summary metrics are printed to stdout and saved to `k6/summary.json`.

---

### Stress Test Results (`k6/summary.json`)

| Metric                             | Measured Value | Threshold | Status |
| :--- | :--- | :--- | :--- |
| **API Response Time** | `p50`: **98.19 ms** \| `p95`: **375.27 ms** \| `avg`: **138.71 ms** \| `max`: **934.64 ms** | `p(95) < 1500 ms` | Pass |
| **Request Throughput** | **101.38 req/s** (`12,726` total requests) | — | — |
| **HTTP Error Rate** | **1.37%** (`0.013673`) | `< 5%` | Pass |
| **Queue Wait Time** | `p50`: **407.50 ms** \| `p95`: **607.00 ms** \| `avg`: **424.83 ms** \| `max`: **781.00 ms** | — | — |
| **Job Completion Duration** | `p50`: **407.50 ms** \| `p95`: **607.00 ms** \| `avg`: **425.95 ms** \| `max`: **1,084.00 ms** | — | — |
| **Jobs Completed** | **276 jobs** | — | — |
| **Job Throughput** | **2.20 jobs/s** | — | — |
| **Job Execution Success Rate** | **100.00%** (`1`) | `> 95%` | Pass |

### Analysis

1. **API Performance & Stability Under Peak Load**
   - At peak stress (`59` concurrent VUs across all three scenarios), the platform sustained **101.38 req/s** across **12,726** HTTP requests with a median latency of **98.19 ms** and a `p95` latency of **375.27 ms**—roughly **4× faster** than the `1,500 ms` `p95` threshold.
   - No HTTP request exceeded 1 second (`max`: **934.64 ms**).
2. **100% Migration Job Reliability**
   - All **276** queued migration jobs reached `SUCCEEDED` (`job_execution_success_rate: 1`), sustaining **2.20 completed jobs/s** with zero failed or timed-out jobs.
3. **Expected Contention vs. Errors (`http_error_rate: 1.37%`)**
   - k6's built-in `http_req_failed` metric counts any non-2xx/3xx status code as an error.
   - During `migration_same_db` (up to `8` concurrent VUs targeting `shop`), concurrent submissions correctly hit the platform's per-database migration lock and receive `409 Conflict`, which accounts for the `1.37%` (~174 requests) rate rather than unexpected `5xx` server errors.
4. **Queue Dispatch & Polling Granularity**
   - `queue_wait_ms` and `job_completion_duration_ms` report identical `p50` (**407.50 ms**) and `p95` (**607.00 ms**) values, with averages differing by only `1.12 ms` (**424.83 ms** vs. **425.95 ms**).
   - Because the client polls `/api/v1/jobs/{id}` every `250 ms` (`POLL_INTERVAL_SEC = 0.25`), observed completion times cluster around 2–3 polling intervals, and once a worker dequeues a job from RabbitMQ, the SQL execution itself completes in a few milliseconds (with an isolated max of `1,084 ms`).
