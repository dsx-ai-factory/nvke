# AGENTS.md

## Project Overview

Cloud Workflow Engine (CWE) orchestrates NKE (NVIDIA Kubernetes Engine) cluster creation and lifecycle operations. The system uses Temporal for workflow orchestration and provides a REST API for cluster management operations.

### Key Technologies
- **Language**: Go 1.26+
- **Workflow Engine**: Temporal
- **Database**: PostgreSQL with Bun ORM
- **API**: OpenAPI 3.0 with oapi-codegen for Go server/client generation
- **Dependencies**: Managed via Flox

## Setup Commands

Before running any CLI commands, activate the Flox environment:
```bash
test "$(flox --version)" = "$(cat .flox-cli-version)" && flox activate
```

Do not activate the environment with a different Flox CLI version. The version
is pinned in `.flox-cli-version`, and `.envrc` skips automatic activation on a
mismatch so a local CLI cannot rewrite the tracked manifest and lockfile.

For non-interactive one-shot commands, prefer running them through Flox directly instead of assuming shell state persists:
```bash
flox activate -- make generate
flox activate -- make test
flox activate -- make lint
```

This is especially important for code generation. Do not invoke `oapi-codegen` directly; always run `make generate` through Flox so the repo's expected toolchain and post-generation steps are used.

### Build
```bash
make build        # Build the worker binary to ./bin/cloud-workflow-engine
```

### Lint
```bash
make lint         # Run golangci-lint (5 minute timeout)
make fmt          # Format code with gofmt and goimports
```

### Test
```bash
make test                  # Run unit tests with coverage
make test-integration      # Run integration tests (requires running CWE deployment, 3h timeout)
```

### Code Generation
```bash
flox activate -- make generate     # Regenerate Go code from OpenAPI spec
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
- Import grouping order: stdlib, then third-party, then local (`github.com/NVIDIA/nke/controller`)
- No `github.com/pkg/errors` - use stdlib `errors` and `fmt.Errorf` with `%w`
- Cyclomatic complexity limit: 15
- Function length limit: 500 lines (excluding comments)

### License Headers
All Go files must have the NVIDIA proprietary license header. Run `make license` to add/update headers automatically.

### Linting Notes
- Use `FIXME:` for things to address in a future MR (with Jira link, e.g., `FIXME: https://jirasw.nvidia.com/browse/NKX-1234`)
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
│   ├── worker/             # Main CWE worker entrypoint
│   └── ui-dev/             # UI development proxy server
├── docs/                   # Developer guides (e.g. remote Colossus Tilt)
├── hack/
│   └── remote-sync/        # Mutagen templates for Mac→Linux Tilt
├── internal/
│   ├── config/             # Configuration from environment variables
│   ├── controllers/        # Background controllers (auto-provisioner, health checks)
│   ├── db/                 # Database layer (Bun ORM, migrations)
│   │   └── migrations/     # Database migration files
│   ├── providers/forge/    # Forge API client with caching
│   ├── server/             # HTTP server and API handlers
│   └── temporal/
│       ├── activities/     # Temporal activity implementations
│       └── workflows/      # Temporal workflow implementations
├── profiles/
│   ├── configProfiles/     # Kubernetes config profiles (YAML)
│   └── osProfiles/         # OS provisioning profiles
├── ui/                     # Static UI files (HTML, JS, CSS)
└── integration-test/       # End-to-end integration tests
```

## API Development

When modifying the API:

1. Edit `api/openapi.yaml`
2. Run `flox activate -- make generate` to regenerate Go code
3. Implement new handlers in `internal/server/`
4. Generated files: `api/models.gen.go`, `api/server.gen.go`, `api/client.gen.go`

**Do not manually edit generated files** - they will be overwritten.

## Database

- ORM: [Bun](https://bun.uptrace.dev/)
- Migrations in `internal/db/migrations/`
- Default connection: `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable`

### Running PostgreSQL locally
```bash
docker run -d --name=cwe-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:17.4
```

## Environment Variables

Key configuration (see `internal/config/config.go` for full list):

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `DB_CONNECTION_STRING` | `postgres://...` | PostgreSQL connection string |
| `TEMPORAL_HOST_PORT` | `localhost:7233` | Temporal server for CWE workflows |
| `FWE_API_KEY` | (required) | NGC API key for Forge API |
| `PROFILES_DIR` | `./profiles` | Directory for OS/config profiles (fleet content supplied at runtime from nke-site-deploy; see NKX-13910) |
| `SPIFFE_ENDPOINT_SOCKET` | (optional) | SPIRE agent socket path for SPIFFE identity (e.g., `unix:///tmp/spire-agent/agent.sock`) |

## Local Development

Preferred day-to-day path is CAPI + Tilt (see root `README.md`). For editing on a
Mac while Tilt/forge-vms runs on a Colossus (or other x86_64 Linux) host, see
[`docs/remote-colossus-tilt.md`](docs/remote-colossus-tilt.md).

### Full Local Setup
1. Start local Temporal: `temporal server start-dev`
2. Start PostgreSQL (see above)
3. Run CWE:
 ```bash
 TEMPORAL_HOST_PORT=localhost:7233 FWE_API_KEY=<NGC_API_KEY> go run ./cmd/worker/main.go
 ```
4. Access UI at http://localhost:8080, Swagger UI at http://localhost:8080/api/docs

### UI-Only Development
To develop UI against a remote CWE deployment:
```bash
kubectl port-forward -n cloud-workflow-engine deploy/cloud-workflow-engine 8080:8080
API_BASE_URL=http://localhost:8080/api go run cmd/ui-dev/main.go
```
UI served at http://localhost:8081.

### SPIRE Integration for Local Development

To test mTLS calls to tenant clusters locally using SPIFFE/SPIRE:
```bash
# Setup and start SPIRE agent (includes port-forward)
make dev-spire-start

# Start CWE with SPIRE
SPIFFE_ENDPOINT_SOCKET=unix:///tmp/spire-agent/agent.sock make run

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
These run against a live CWE deployment and use real Forge resources:
```bash
cd integration-test
CWE_API_URL=http://localhost:8080/api go test -v -timeout=3h
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

Releases are **fully automatic on merge to `main`** — there is no manual
version tag, image push, chart bump, or `nke-site-deploy` edit to make. The
CI pipeline (`.gitlab/ci/`) handles the whole chain:

1. **`gen-version`** derives `BUILD_VERSION` from `git describe`
   (auto-incrementing `v0.0.x`).
2. **`publish-image-nvcr`** builds and pushes the multi-arch image to
   `nvcr.io/j7sbcjl3qgta/cloud-workflow-engine:<BUILD_VERSION>`.
3. **`publish-chart-nvcr`** packages `deploy/charts/cloud-workflow-engine`
   with `version`/`appVersion` derived from `BUILD_VERSION` and pushes it to
   `oci://nvcr.io/j7sbcjl3qgta/cloud-workflow-engine:<BUILD_VERSION>`. The
   chart and image are versioned 1:1.
4. **`update-environment-templates`** triggers the `nke/nke-site-deploy`
   `main` pipeline, which auto-bumps the environment templates to the new
   `BUILD_VERSION`. Those templates are the source of truth ArgoCD rolls out
   to the site-controller fleet.

`forge-console` (UI + `/api/forge/*`) lives in `nke/forge-console` and publishes
its own image/chart + `FORGE_CONSOLE_CHART_VERSION` bump. Local Tilt loads it
from `../forge-console` or `FORGE_CONSOLE_DIR`.

Because the environment templates auto-bump to latest on every merge, MR
review is the gate: feature branches must be well tested before merge. To
ship a change (e.g. a CVE-reduction dependency bump), merge the MR to `main`
and the new image + chart reach the fleet without further action; confirm
the result on the scanning/deploy dashboard afterward.

## PR Guidelines

- Run `make lint` and `make test` before committing
- Ensure all `TODO:` comments are resolved
- Add license headers: `make license`
- Regenerate API code if OpenAPI spec changed: `flox activate -- make generate`

### MR Format
- **Title**: `JIRA-KEY: Brief description` (e.g., `NKE-1234: Add node pool scaling`)
- **Description**: Start with `Closes JIRA-KEY`, followed by a summary of changes

