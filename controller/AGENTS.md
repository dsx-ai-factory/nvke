# AGENTS.md

## Project Overview

Cloud Workflow Engine (CWE) orchestrates NVKE (NVIDIA Kubernetes Engine) cluster creation and lifecycle operations. The system uses Temporal for workflow orchestration and provides a REST API for cluster management operations.

### Key Technologies
- **Language**: Go — the floor is the `go` directive in the root `go.work`, not
  this module's `go.mod`. `controller/go.mod` is lower, but every build resolves
  through the workspace. Read the current value with
  `awk '/^go /{print $2; exit}' ../go.work` rather than trusting a number here.
  Under the default `GOTOOLCHAIN=auto` an older local Go downloads a conforming
  toolchain and the build just works; it only fails with
  `go.work requires go >= <version>` when that switch is blocked
  (`GOTOOLCHAIN=local`, an explicit older pin, or no network to fetch one).
  Editors and language servers hit that first, because they call `go list`
  directly rather than through `make`, which pins `GOTOOLCHAIN` for you.
- **Workflow Engine**: Temporal
- **Database**: PostgreSQL with Bun ORM
- **API**: OpenAPI 3.0 with oapi-codegen for Go server/client generation
- **Dependencies**: Managed via Flox

## Architecture invariants — read before touching any handler, reconciler, or workflow

### One process, three programming models

A single `worker` binary runs three distinct subsystems concurrently:

- **REST API** (`internal/server/`) — HTTP handlers backed by Postgres
- **Temporal worker** (`internal/temporal/`) — workflow and activity implementations
- **Controllers** (`internal/controllers/`) — a mix of k8s controller-runtime reconcilers and postgres state reconcilers for various tasks

Reading only one subsystem gives an incomplete picture of what reacts to a change.
Any change that touches cluster or node state should be checked against all three.

### The outbox pattern — never call Temporal from a handler

The API does not start workflows. A handler should not call Temporal directly.

The correct flow:
1. The handler opens one database transaction and writes the cluster record *and* a row in `workflow_outbox` — both land or neither does (`internal/server/clusters.go`, `CreateWorkflowOutboxEntry`)
2. `OutboxProcessor` polls `workflow_outbox`, starts the Temporal workflow, and marks the row processed (`internal/temporal/outbox_processor.go`)

This indirection is deliberate. It guarantees that a committed cluster always has its work queued, with no window where the cluster record exists but the workflow does not.

## Setup Commands

Before running any CLI commands, activate the Flox environment. It is
repository-wide and lives at the repository root, so `flox activate` works from
here by searching upwards; only the version check needs the root path:
```bash
root="$(git rev-parse --show-toplevel)"
test "$(flox --version)" = "$(cat "${root}/.flox-cli-version")" && flox activate
```

Do not activate the environment with a different Flox CLI version. The version
is pinned in `.flox-cli-version` at the repository root, and the root `.envrc`
skips automatic activation on a mismatch so a local CLI cannot rewrite the
tracked manifest and lockfile.

For non-interactive one-shot commands, prefer running them through Flox directly instead of assuming shell state persists:
```bash
flox activate -- make generate
flox activate -- make test
flox activate -- make lint
```

For code generation, do not invoke `oapi-codegen` directly; run `make generate`. The generator version is pinned in `api/oapi-codegen.mod` and run through `go generate`, so the output does not depend on whatever is on `PATH`. Flox is optional here as everywhere, but prefer `flox activate -- make <target>` when `flox` is available and you are not already inside the environment.

### Build
```bash
make build        # Build the worker binary to ./bin/controller
```

### Lint
```bash
make lint         # Run golangci-lint (20 minute timeout)
make fmt          # Format code with gofumpt and goimports
```

### Test
```bash
make test                  # Run unit tests with coverage
make test-integration      # Run integration tests (requires running CWE deployment, 3h timeout)
```

### Code Generation
```bash
make generate     # Regenerate Go code from OpenAPI spec
make license      # Add/update license headers on code files
```

### Run Locally
```bash
make run          # Run the worker
make debug        # Run with delve debugger on :12345
```

## Code Style

### Go Conventions
- Use `go fmt` and `goimports` for formatting
- Import grouping order: stdlib, then third-party, then local (`github.com/dsx-ai-factory/nvke/controller`)
- No `github.com/pkg/errors` - use stdlib `errors` and `fmt.Errorf` with `%w`
- Cyclomatic complexity limit: 15
- Function length limit: 500 lines (excluding comments)

### License Headers
All Go files must use the repository's Apache-2.0 SPDX header. Run `make license` to add missing headers.

### Prometheus metric naming
All metric names use the `nvke_controller_` prefix — never `cwe_` (legacy) (e.g. `nvke_controller_node_provision_total`, not `cwe_node_provision_total`). Metrics live in `internal/metrics/` and `internal/dbmetrics/`.

### Linting Notes
- Use `FIXME:` for things to address in a future PR, with a tracking link
- `TODO:` comments will cause lint failures - resolve before merging
- Test files have relaxed linting rules
- Workflow files (`internal/temporal/workflows/`) allow naked returns and named returns

## Project Structure

```
├── api/                    # OpenAPI spec and generated Go code
│   ├── openapi.yaml        # OpenAPI 3.0 specification (source of truth)
│   ├── models.gen.go       # Generated models
│   ├── server.gen.go       # Generated server interfaces
│   └── client.gen.go       # Generated client
├── cmd/
│   └── worker/             # Main CWE worker entrypoint
├── deploy/dashboards/      # Grafana reference JSON (import-only; see deploy/dashboards/AGENTS.md)
├── docs/                   # Developer guides (API authentication, BYO OS)
├── hack/
│   └── remote-sync/        # Mutagen templates for Mac→Linux Tilt
├── internal/
│   ├── config/             # Configuration: environment variables and nke-controller-config fields
│   ├── controllers/        # Background controllers (auto-provisioner, health checks)
│   ├── db/                 # Database layer (Bun ORM, migrations)
│   │   └── migrations/     # Database migration files
│   ├── server/             # HTTP server and API handlers
│   └── temporal/
│       ├── activities/     # Temporal activity implementations
│       └── workflows/      # Temporal workflow implementations
├── profiles/
│   └── configProfiles/     # Kubernetes config profiles (YAML)
└── integration-test/       # End-to-end integration tests
```

## API Development

When modifying the API:

1. Edit `api/openapi.yaml`
2. Run `make generate` to regenerate Go code
3. Implement new handlers in `internal/server/`
4. Generated files: `api/models.gen.go`, `api/server.gen.go`, `api/client.gen.go`

**Do not manually edit generated files** - they will be overwritten.

Two gates cover a spec change, and they answer different questions:

```bash
make generate-check    # the committed *.gen.go match api/openapi.yaml
make api-compat-check  # the openapi.yaml change does not break an existing client
```

A removed field or a newly required property passes the first and fails the
second. `api-compat-check` compares against the newest published
`controller/api/vX.Y.Z` tag and is waived, never disabled, per change in
`api/openapi-compat-waivers.yaml`.

Removing an operation is the exception, and the one agents get wrong: do not
write a waiver for it. Mark it `deprecated: true` with an `x-sunset` date at
least 180 days out, ship that, and remove it once the date has passed — the gate
lets that through on its own. A deprecation with no `x-sunset`, or one dated
sooner than that, fails.

See [`api/README.md`](api/README.md#compatibility) and
[`hack/openapi-compat/README.md`](hack/openapi-compat/README.md).

## Database

- ORM: [Bun](https://bun.uptrace.dev/)
- Migrations in `internal/db/migrations/`
- Default connection: `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable`

### Running PostgreSQL locally
```bash
docker run -d --name=cwe-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:17.4
```

## Configuration

The controller reads its settings once, at startup, from the
`nke-controller-config` file (`internal/controllerconfig`), and reads a few more
from environment variables (`internal/config/config.go`). The chart renders the
file into a ConfigMap and mounts it; host-local runs use
`hack/local-controller-config.yaml`. A field of `config.Config` with a
`controllerconfig` tag comes from the file, and one with an `env` tag from the
environment.

| Variable | Default | Description |
|----------|---------|-------------|
| `CONTROLLER_CONFIG_PATH` | (required) | The `nke-controller-config` file. The chart sets it to the mounted ConfigMap; `make run` sets it to `hack/local-controller-config.yaml`. Unset, missing or invalid is a startup failure. |
| `LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `DB_CONNECTION_STRING` | `postgres://...` | PostgreSQL connection string |
| `AUTH_CONFIG_PATH` | (unset) | API authentication config file. Unset disables authentication entirely, which is what `make run` gets. Set but unreadable is a startup failure, never "disabled". |

Settings in the file that local work most often changes:

| Key | Description |
|-----|-------------|
| `temporal.hostPort` | Temporal server for controller workflows |
| `profilesDir` | Directory for config profiles and the runbooks their hooks reference. Fleet profile content is maintained outside this repository and supplied at deploy time; this repo ships only example profiles. The directory may be empty: the controller starts without a profiles tree, and one is needed to provision a node, not to come up. |
| `spire.spiffeEnabled`, `spire.spiffeEndpointSocket` | SPIFFE identity, from the SPIRE agent socket (for example `unix:///tmp/spire-agent/agent.sock`) |
| `permissiveMode` | Only read when `AUTH_CONFIG_PATH` is set. `true` evaluates, logs and counts every request, then serves it anyway — nothing is rejected. |

To change one for your own runs, copy `hack/local-controller-config.yaml` under
`.local/` and point `CONTROLLER_CONFIG_PATH` at the copy.

## Local Development

Preferred day-to-day path is CAPI + Tilt (see `README.md` in this directory).

### Full Local Setup
1. Start local Temporal: `temporal server start-dev`
2. Start PostgreSQL (see above)
3. Install the release CRDs and the fixture releases into the cluster in `KUBECONFIG`.
 CWE exits at startup without the CRDs, and provisioning fails without the releases:
 ```bash
 make releases-install
 ```
4. Run CWE, with `hack/local-controller-config.yaml`:
 ```bash
 make run
 ```
5. Access Swagger UI at http://localhost:8080/api/docs.

### API Authentication

The two local paths differ, and that is the usual source of confusion:

| Path | Authentication |
|------|----------------|
| `make run` | **off** — `AUTH_CONFIG_PATH` is unset, every request is served |
| `tilt up` | **enforcing** — `/api/*` needs a bearer token |

So a bare `curl` that works host-local returns 401 under Tilt. `nvkectl` mints
from the kind kubeconfig and does not need a token environment variable:

```bash
nvkectl config set-site --name local \
  --url http://localhost:8080 \
  --kubeconfig ~/.kube/config
nvkectl get clusters
```

For `curl`, get a token:

```bash
TOKEN="$(hack/nvke-token.sh)"
curl -H "Authorization: Bearer ${TOKEN}" http://localhost:8080/api/clusters
```

`hack/nvke-token.sh` prints nothing when authentication is off and exits 0, so one
form works on both paths — the `${TOKEN:+...}` expansion drops the header
entirely rather than sending an empty one:

```bash
TOKEN="$(hack/nvke-token.sh)"
curl ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} http://localhost:8080/api/clusters
```

`NKE_AUTH_ENABLED=false tilt up` turns it off. `NKE_PERMISSIVE_MODE=true tilt up`
evaluates and counts without rejecting anything.

`/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`, `/openapi-public.yaml`, `/docs` and `/api/docs/`
never need a token. The Swagger UI therefore loads either way, but calls issued
from it 401 under Tilt.

Full walkthrough, including what each rejection means:
[`docs/authenticating-to-the-api.md`](docs/authenticating-to-the-api.md).
For configuring authentication on a real deployment, see
[`docs/configuring-api-authentication.md`](docs/configuring-api-authentication.md).

### SPIRE Integration for Local Development

To test mTLS calls to tenant clusters locally using SPIFFE/SPIRE:
```bash
# Setup and start SPIRE agent (includes port-forward)
make dev-spire-start

# Start CWE with SPIRE: a copy of the local config with spire.spiffeEnabled set
mkdir -p .local
yq '.spire.spiffeEnabled = true' hack/local-controller-config.yaml > .local/controller-config.spire.yaml
CONTROLLER_CONFIG_PATH=.local/controller-config.spire.yaml make run

# Stop agent
make dev-spire-stop

# Clean up (removes config, data, logs)
make dev-spire-clean
```

## Testing

### Unit Tests
```bash
make test
```

### Integration Tests
These run against a live CWE deployment:
```bash
cd integration-test
NVKE_CONTROLLER_API_URL=http://localhost:8080/api go test -tags integration_test -v -timeout=3h
```

## Temporal Workflows

Workflows are in `internal/temporal/workflows/`:
- `provision_cluster.go` - Cluster provisioning workflow
- `provision_node_pool.go` - Node pool provisioning
- `provision_node.go` - Individual node provisioning
- `teardown_*.go` - Teardown workflows
- `reboot_node.go` - Node reboot handling

Activities are in `internal/temporal/activities/`.

## Release & Deployment

Controller releases use signed `controller/vX.Y.Z` tags. GitHub Actions builds
the release and publishes the controller image and Helm chart. Follow the root
[`RELEASE.md`](../RELEASE.md); do not create releases by hand.

## Reference Grafana dashboards

JSON under `deploy/dashboards/` is imported by operators (see [deploy/dashboards/README.md](deploy/dashboards/README.md)). It is not rendered by Helm. Before editing those files, read [deploy/dashboards/AGENTS.md](deploy/dashboards/AGENTS.md).

## PR Guidelines

- Run `make lint` and `make test` before committing
- Ensure all `TODO:` comments are resolved
- Add license headers: `make license`
- Regenerate API code if OpenAPI spec changed: `make generate`
- Check the spec change is backward compatible: `make api-compat-check`

### PR Format
- Use a Conventional Commit title, such as `fix: handle node reset timeout`
- Reference the issue with `Closes #<number>` when applicable
