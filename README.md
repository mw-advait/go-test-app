### To start the go app for performance issues

1. Run - `docker compose up --build`
2. To test - `bash run-tests.sh --mw-only p3` ( p3 - slow db query )

### How to test

Both apps run side by side: MW on `localhost:3011`, Sentry on `localhost:3012`.

```bash
bash run-tests.sh [--mw-only|--sentry-only] [section]
```

| Section | What it runs |
|---|---|
| *(none)* / `all` | debug + all perf rules + all baseline |
| `perf` | Performance rules P1–P13 |
| `baseline` | Existing MW detections M1–M4 |
| `debug` | Debug endpoints only (Sentry tracing check) |
| `p1` … `p13`, `m1` … `m4` | Single rule |

#### Performance rules (`perf`)

| ID | Rule | Command | Endpoint | Threshold |
|---|---|---|---|---|
| P1 | N+1 queries | `bash run-tests.sh --mw-only p1` | `/test/n-plus-one?count=20` | 5+ spans, total > 100ms |
| P2 | Consecutive DB queries | `bash run-tests.sh --mw-only p2` | `/test/consecutive-db` | savings > 100ms, each > 30ms |
| P3 | Slow DB query | `bash run-tests.sh --mw-only p3` (or `./run-slow-db-query.sh [count]`) | `/test/slow-db` | SELECT >= 500ms, 100+ times in 24h |
| P4 | Large HTTP payload | `bash run-tests.sh --mw-only p4` (or `./run-large-http-payload.sh [count] [size_kb] [delay_ms]`) | `/test/large-payload?size=400&delay=150` | > 300KB AND > 100ms |
| P5 | Endpoint regression | no `run-tests.sh` section; call manually | `/test/slow-endpoint` | p95 regression vs baseline |
| P6 | Consecutive HTTP calls | `bash run-tests.sh --mw-only p6` | `/test/consecutive-http` | 5 sequential outbound calls |
| P7 | DB connection leak | `bash run-tests.sh --mw-only p7` | `/test/db-connection-leak?count=3&hold=5000` | 3 connections held 5s |
| P8 | Long transaction | `bash run-tests.sh --mw-only p8` | `/test/long-transaction` | locks held ~800ms |
| P9 | Nested N+1 | `bash run-tests.sh --mw-only p9` | `/test/nested-n-plus-one` | users → orders, 10+10 queries |
| P10 | Retry storm | `bash run-tests.sh --mw-only p10` | `/test/retry-storm?retries=5` | 5 retries, no backoff |
| P11 | N+1 HTTP calls | `bash run-tests.sh --mw-only p11` | `/test/n-plus-one-http?count=15` | 15 sequential identical GETs |
| P12 | Slow outbound HTTP | `bash run-tests.sh --mw-only p12` | `/test/slow-http?delay=2000` | >= 500ms |
| P13 | Uncompressed response | `bash run-tests.sh --mw-only p13` | `/test/uncompressed-response?size=600` | > 512KB without gzip |

#### Baseline: existing MW detections (`baseline`)

| ID | Detection | Command | Endpoint | Variants |
|---|---|---|---|---|
| M1 | Runtime exceptions (`exception.message` on span) | `bash run-tests.sh --mw-only m1` | `/test/existing/exception-message?variant=<v>` | `null-ref`, `async-reject`, `deep-stack`, `db-error` |
| M2 | Business errors | `bash run-tests.sh --mw-only m2` | `/test/existing/business-error?scenario=<s>` | `payment`, `auth`, `validation`, `timeout` |
| M3 | High tail latency | `bash run-tests.sh --mw-only m3` | `/test/existing/high-latency?delay=3000` | 3s+ requests |
| M4 | Log errors | `bash run-tests.sh --mw-only m4` | `/test/existing/log-errors?count=20` | 20 error logs |

Each section sends one request per variant, then 4 more of each as a bulk run.
