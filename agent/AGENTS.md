# AGENTS.md

## Project Overview

The **NVKE agent** lives at `agent/` in the [dsx-ai-factory/nvke](https://github.com/dsx-ai-factory/nvke) monorepo.
It is a Go-based gRPC server (systemd service) that runs on Kubernetes nodes and provides remote management for Kubeadm and software package/bundle installation.

**Primary use cases:**
- Kubernetes cluster creation and management via Kubeadm (init, join, reset)
- Node bootstrapping and software package/bundle installation
- Kubernetes API/etcd secrets encryption
- mTLS via SPIFFE (SPIRE workload API)

**Module path:** `github.com/dsx-ai-factory/nvke/agent`

## Consumer configuration

Copy `config.env.example` → `config.env` (gitignored) to point builds and the agent
at **your** artifact hosts. See that file for `NKE_BUNDLE_SERVER_URL`, apt intake,
and container registry knobs. Runtime still accepts the same values as CLI flags
(`--bundle-server-url`, etc.); env vars populate flag defaults.

## Versioning

Agent releases use **component-scoped git tags** (Kubernetes/Go multi-module monorepo style):

```bash
git tag -a agent/v0.1.30 -m "agent v0.1.30"
git push origin agent/v0.1.30
```

Pushing the tag publishes:

- OCI binaries to GHCR / NVCR (`.github/workflows/agent-artifact*.yml`)
- `.deb`s to the apt-publisher S3 intake, as assets on the GitHub Release
  for that tag, and to GHCR as `ghcr.io/dsx-ai-factory/nvke/agent-deb`
  (`.github/workflows/agent-deb-publish.yml`)

`make` / `./build/agent.sh` derive `VERSION` from:

```bash
git describe --match 'agent/v*' --tags --always --dirty
```

The `agent/` prefix is stripped for the binary ldflag and `.deb` version
(`nvke-agent_0.1.30_amd64.deb`). Override with `VERSION=v0.1.30` when needed.

Go module consumers pin the same tag shape:

```bash
go get github.com/dsx-ai-factory/nvke/agent@agent/v0.1.30
```

## Build Commands

From the monorepo root:

```bash
./build/agent.sh           # build all arches
./build/agent.sh test
./build/agent.sh package
./build/agent.sh protos
```

Or from `agent/`:

```bash
# Build all architectures (amd64 + arm64)
make build

# Build specific architecture
make build-amd64
make build-arm64

# Package release bundles
make package

# Clean build artifacts
make clean
```

## Test Commands

```bash
# Run all tests
make test

# Run tests directly
go test ./...

# Run a specific test
go test -v ./internal/agent -run TestExecuteKubeadmInit
```

## Code Quality

```bash
make fmt
make vet
make check-protos
make lint
make notices-check
```

`make lint` is the CI lint gate. `make notices-check` verifies that
`THIRD_PARTY_NOTICES.md` matches the shipped dependency set.

## Proto Generation

This project uses Protocol Buffers for gRPC definitions. Proto files are located in `pkg/*/proto/`.

```bash
# Generate proto files (local buf)
make protos

# Format proto files
make proto-fmt

# Verify proto files are up to date
make check-protos

# Clean generated proto files
make clean-proto
```

**Important:** After modifying `.proto` files, always run `make protos` and commit the updated `*.pb.go` files together with proto changes.

## Git Hooks

```bash
# Install pre-commit hook into the monorepo .git/hooks (auto-verifies protos)
make setup-hooks
```

The pre-commit hook automatically:
1. Verifies proto files are up to date
2. Regenerates and stages them if outdated

## Project Structure

```text
cmd/
└── agent/          # Main nvke-agent binary

internal/
├── agent/          # Core agent server implementation
├── grpcserver/     # gRPC registration / reflection
└── k8s/            # Kubernetes command utilities

pkg/
├── agent/proto/    # NodeAgent gRPC proto definitions
├── auth/           # Token interceptor for gRPC auth
├── deb/            # Debian package helpers
└── utils/          # Shared utilities (bundles, cmd-runner, etc.)

scripts/            # Installation scripts bundled with releases
samples/            # Sample configurations and clients
docs/               # Design and security context docs
```

## Code Conventions

### Go Style
- Use `slog` for structured logging (not `log` or `fmt.Printf`)
- Error messages should be lowercase and not end with punctuation
- Use table-driven tests with descriptive test case names
- Prefer interfaces for testability (see `CommandRunner`, `FileWriter`, `NodeStatusPersister`)

### Naming
- gRPC methods use PascalCase matching proto definitions
- Internal status constants are lowercase strings: `"ready"`, `"pending"`, `"error"`
- Node roles: `"controlplane"`, `"worker"` (lowercase)

### Error Handling
- Return gRPC status errors for RPC methods: `status.Errorf(codes.Internal, "...")`
- Use `fmt.Errorf` with `%w` for error wrapping
- Log errors before returning with `slog.Error`

### Testing
- Use `github.com/stretchr/testify/assert` for assertions
- Create mock implementations of interfaces for unit tests
- Test files are colocated with source files (`*_test.go`)

## Key Files

| File | Purpose |
|------|---------|
| `internal/agent/agent.go` | Main NodeAgent gRPC server implementation |
| `internal/agent/status.go` | Node status management and persistence |
| `pkg/agent/proto/node_agent.proto` | gRPC service definitions |
| `pkg/utils/bundles.go` | Bundle fetching and installation logic |
| `pkg/utils/cmd-runner.go` | Command execution abstraction |

## Dependencies

- **Go** — the floor is the `go` directive in the root `go.work`, not this
  module's `go.mod`. `agent/go.mod` is lower, but every build resolves through
  the workspace. Read the current value with
  `awk '/^go /{print $2; exit}' ../go.work` rather than trusting a number here.

  Under the default `GOTOOLCHAIN=auto` an older local Go downloads a conforming
  toolchain and the build just works, so this is usually invisible. It bites
  when that switch is blocked — `GOTOOLCHAIN=local`, an explicit older pin, or
  no network to fetch one — and then every workspace-aware command, `go list`
  included, fails with `go.work requires go >= <version>`. Editors and language
  servers hit this first, because they call `go list` directly rather than
  through `make`, which pins `GOTOOLCHAIN` to the `go.work` value for you.
- **buf** (for proto generation; see `BUF_VERSION` in `make/tool-versions.mk`)
- **protoc** plugins via buf remote plugins (`protoc-gen-go`, `protoc-gen-go-grpc`)

## State Management

The agent persists state to `/var/lib/nvke/state.json`. Key state includes:
- Node role and phase (controlplane/worker, join/init/reset)
- Kubeadm status and join strings
- Bundle installation status
- Disk encryption status

## Common Patterns

### Adding a new gRPC method

1. Add the method definition to `pkg/agent/proto/node_agent.proto`
2. Run `make protos`
3. Implement the method in `internal/agent/agent.go` (or the appropriate domain file)
4. Add unit tests in the colocated `*_test.go`

### Background operations

Long-running operations use goroutines and update `NodeStatus`:
```go
go s.processKubeadmInit(args, req) // Async processing
defer s.flushStatus(&ns) // Persist on completion
```

### Testing with mocks

```go
server := &NodeAgentServer{
  Runner: &MockRunner{Output: []byte("..."), Err: nil},
  FileWriter: &MockFileWriter{},
  Persister: &MockPersister{},
  NodeStatus: &NodeStatus{...},
}
```

## gRPC server reflection

The agent registers **gRPC server reflection** (v1 and v1alpha) on the same port as `proto.NodeAgent`
(see `internal/grpcserver.Register`). Tools such as `grpcurl` can list services and invoke RPCs
without a local copy of `node_agent.proto`:

```bash
# Plaintext (development)
grpcurl -plaintext localhost:15005 list
grpcurl -plaintext localhost:15005 list proto.NodeAgent

# Readiness check (used by scripts/grpcurl-lib.sh)
grpcurl -plaintext localhost:15005 list
```

Orchestrators (the NVKE controller) probe for RPC availability via reflection before calling
apt or InstallCNI workflows. Semver in `GetKubeadmStatus` remains useful for logging only.

## Security Notes

- Vulnerability reporting: see the monorepo root `SECURITY.md`. Node-agent architecture notes: `docs/SECURITY.md`.
- With mTLS enabled (default), the agent uses SPIFFE identities from the workload API (`--spiffe-socket-path`). Use `--disable-mtls` only for local development (plaintext gRPC).
- Reflection is enabled on the same mTLS-protected server; clients need valid SPIFFE credentials to use it in production.
- Encryption provider: secretbox (XSalsa20-Poly1305) for Kubernetes API secrets
