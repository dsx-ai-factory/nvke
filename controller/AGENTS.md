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

This is especially important for code generation. Do not invoke `oapi-codegen` directly; always run `make generate` through Flox so the repo's expected toolchain and post-generation steps are used.

### Build
```bash
make build        # Build the worker binary to ./bin/cloud-workflow-engine
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
All Go files must use the repository's Apache-2.0 SPDX header. Run `make license` to add missing headers.

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
| `AUTH_CONFIG_PATH` | (unset) | API authentication config file. Unset disables authentication entirely, which is what a host-local `go run` gets. Set but unreadable is a startup failure, never "disabled". |
| `PERMISSIVE_MODE` | `false` | Only read when `AUTH_CONFIG_PATH` is set. `true` evaluates, logs and counts every request, then serves it anyway — nothing is rejected. |

## Local Development

Preferred day-to-day path is CAPI + Tilt (see root `README.md`).

### Full Local Setup
1. Start local Temporal: `temporal server start-dev`
2. Start PostgreSQL (see above)
3. Run CWE:
 ```bash
 TEMPORAL_HOST_PORT=localhost:7233 FWE_API_KEY=<NGC_API_KEY> go run ./cmd/worker/main.go
 ```
4. Access Swagger UI at http://localhost:8080/api/docs.

### API Authentication

The two local paths differ, and that is the usual source of confusion:

| Path | Authentication |
|------|----------------|
| `make run` / `go run ./cmd/worker/main.go` | **off** — `AUTH_CONFIG_PATH` is unset, every request is served |
| `tilt up` | **enforcing** — `/api/*` needs a bearer token |

So a bare `curl` that works host-local returns 401 under Tilt. Get a token:

```bash
TOKEN="$(hack/nke-token.sh)"
curl -H "Authorization: Bearer ${TOKEN}" http://localhost:8080/api/clusters
```

`hack/nke-token.sh` prints nothing when authentication is off and exits 0, so one
form works on both paths — the `${TOKEN:+...}` expansion drops the header
entirely rather than sending an empty one:

```bash
TOKEN="$(hack/nke-token.sh)"
curl ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} http://localhost:8080/api/clusters
```

`NKE_AUTH_ENABLED=false tilt up` turns it off. `NKE_PERMISSIVE_MODE=true tilt up`
evaluates and counts without rejecting anything.

`/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`, `/docs` and `/api/docs/`
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
CWE_API_URL=http://localhost:8080/api go test -tags integration_test -v -timeout=3h
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

## PR Guidelines

- Run `make lint` and `make test` before committing
- Ensure all `TODO:` comments are resolved
- Add license headers: `make license`
- Regenerate API code if OpenAPI spec changed: `flox activate -- make generate`

### PR Format
- Use a Conventional Commit title, such as `fix: handle node reset timeout`
- Reference the issue with `Closes #<number>` when applicable
