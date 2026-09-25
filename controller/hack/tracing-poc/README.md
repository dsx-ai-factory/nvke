# CWE tracing POC

This POC connects **CWE HTTP → PostgreSQL transaction/outbox → Temporal workflow
→ activity → PostgreSQL**, with one trace and structured success/failure logs.
The test uses the real `POST /api/clusters` endpoint, production database queries,
migrations, outbox processor, and Temporal client. A test-only `ProvisionCluster`
workflow reads the cluster record and injects controlled outcomes. It does not
provision machines or require Kubernetes, Forge, SPIRE, or cloud credentials.

From `controller/`, with Docker running and the repository's Go toolchain:

```bash
bash hack/tracing-poc/run.sh up
bash hack/tracing-poc/run.sh test
```

The runner uses plain Docker; Compose is not required. Alternatively,
`docker compose -f hack/tracing-poc/compose.yaml up -d --wait` starts the same
services. Choose one runner; both bind the same ports.

| Service | Local address |
|---|---|
| PostgreSQL, dedicated `cwe_tracing_poc` database | `127.0.0.1:15432` |
| Temporal gRPC / UI | `127.0.0.1:17233` / <http://localhost:18233> |
| Jaeger OTLP gRPC / UI | `127.0.0.1:14317` / <http://localhost:16686> |

The API listens on an ephemeral localhost port for the duration of the test.
The test creates uniquely named cluster records and uses a unique task queue.
Only the dedicated POC database is migrated. The containers remain running for
inspection. The PostgreSQL container uses local development trust authentication;
all published ports bind to loopback.

## What the test verifies

- Valid incoming W3C context is preserved; malformed or absent context produces
  a new trace. Responses expose `X-Trace-ID` and server-generated `X-Operation-ID`.
- The same database transaction stores resource changes and outbox trace context.
- A background outbox consumer restores the context after the HTTP request ends.
- HTTP, SQL, workflow-start, workflow-run, and activity-run spans share a trace.
- A transient failure produces two activity attempts and one successful workflow;
  a non-retryable failure produces a failed workflow. Temporal's recorded result
  is checked independently of logs.
- API, SQL, workflow, and activity logs carry matching correlation fields.
- Temporal tracing is registered once: the worker inherits the client interceptor.

The test prints a Jaeger link, workflow ID, and operation ID for each scenario.
Jaeger service name: `cwe-tracing-poc`. Look for:

```text
POST
├── INSERT / UPDATE / transaction spans
└── outbox.dispatch
    └── StartWorkflow:ProvisionCluster
        └── RunWorkflow:ProvisionCluster
            └── StartActivity:POCReadCluster
                └── RunActivity:POCReadCluster (one per attempt)
                    └── SELECT
```

Logs are written to `.local/tracing-poc/logs.jsonl` and replaced on each run.
For example, copy an operation ID printed by the test:

```bash
jq -c --arg id 'PASTE_OPERATION_ID' \
  'select(.operation_id == $id)' .local/tracing-poc/logs.jsonl

jq -c 'select(.event == "workflow.returned" or .event == "activity.returned") |
  {trace_id, operation_id, workflow_id, run_id, activity_id, attempt, outcome, error}' \
  .local/tracing-poc/logs.jsonl
```

## Using the changes with a full local CWE instance

Configure the existing worker with these tracing variables and run it using your
normal local configuration:

```bash
export OTEL_TRACING_ENABLED=true
export OTEL_EXPORTER_OTLP_ENDPOINT=127.0.0.1:14317
export OTEL_EXPORTER_OTLP_INSECURE=true
```

The worker applies the additive `workflow_outbox.trace_context` migration on
startup. It stores W3C `traceparent`, optional `tracestate`, and `operation_id` in
a JSONB column. Existing rows with NULL context still dispatch normally.
`operation_id` is carried across Temporal headers independently of sampling.
Tracing still requires the existing enable flag and OTLP endpoint; this POC does
not change deployment defaults. Requests still receive an operation ID when
tracing is disabled.

The standard worker logger adds `trace_id`, `span_id`, and `operation_id` to
context-aware calls. Use `logger.InfoContext(ctx, ...)` in Go code and
`workflow.GetLogger(ctx)` / `activity.GetLogger(ctx)` in Temporal code. Legacy
context-free `slog.Info(...)` calls cannot infer the current request automatically.

## Scope and outcome semantics

This POC covers API-triggered outbox workflows and their Temporal descendants.
Separate controller/reconciler starts, such as later node-scheduling decisions,
still need their own durable context handoff. Node-agent gRPC instrumentation,
CLI tracing, and a hosted log backend are not part of this POC.

`http.completed` describes the HTTP response, not provisioning completion.
`activity.returned` describes one activity attempt. `workflow.returned` describes
a workflow function return for a run; it uses Temporal's replay-aware logger.
Continue-as-new is recorded as `continued_as_new`. It is **not a durable audit
record**: worker crashes, workflow task failures, server-side timeout/termination,
and workflow retries can prevent or qualify a worker-side log. Temporal history
and execution status remain authoritative. A production operation-status monitor
would be needed to publish every terminal server-side outcome reliably.

The POC uses one trace across the outbox boundary. Trace sampling can discard
spans; operation IDs and outcome logs remain available independently. There is
no automatic retry-wide error retention policy in this POC.

To stop the plain-Docker services without removing their containers:

```bash
bash hack/tracing-poc/run.sh stop
```

For the Compose alternative, use `docker compose -f hack/tracing-poc/compose.yaml stop`.
Jaeger and the Temporal dev server use in-memory storage, so stopping them loses
their traces/history. The JSON log file remains on disk.

## CI logs

The `Controller Unit Tests` workflow runs **Tracing POC (API to Temporal)** as
part of the PR merge gate, including draft PRs. Expand **Show correlated request
and workflow logs** to compare HTTP/SQL/workflow/activity records by operation
ID. The job summary includes activity attempts and workflow outcomes.

The `cwe-tracing-poc-<run>-<attempt>` artifact contains `logs.jsonl`, test output,
Jaeger `traces.json`, and local service diagnostics. These come from an isolated
synthetic test environment with no cloud credentials or real customer requests.
Localhost Jaeger links in test output apply to local runs; use the exported JSON
trace artifact when inspecting a CI run.
