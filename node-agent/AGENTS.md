# AGENTS.md

## Project Overview

The **NKE Node-Agent** lives at `node-agent/` in the [NVIDIA/nke](https://github.com/NVIDIA/nke) monorepo.
It is a Go-based gRPC server (systemd service) that runs on Kubernetes nodes and provides remote management for Kubeadm, full disk encryption (FDE), and software package/bundle installation.

**Primary use cases:**
- Kubernetes cluster creation and management via Kubeadm (init, join, reset)
- Node bootstrapping and software package/bundle installation
- Full disk encryption with TPM unlock
- Kubernetes API/etcd secrets encryption
- mTLS via SPIFFE (SPIRE workload API)

**Module path:** `github.com/NVIDIA/nke/node-agent`

## Build Commands

From the monorepo root:

```bash
./build/node-agent.sh           # build all arches
./build/node-agent.sh test
./build/node-agent.sh package
./build/node-agent.sh protos
```

Or from `node-agent/`:

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
# Format code
make fmt
# or
go fmt ./...

# Run static analysis
make vet
# or
go vet ./...
```

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
└── agent/          # Main nke-agent binary

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

- **Go 1.24+** (see `go.mod`)
- **buf** (for proto generation; see `BUF_VERSION` in Makefile)
- **protoc** plugins via buf remote plugins (`protoc-gen-go`, `protoc-gen-go-grpc`)

## State Management

The agent persists state to `/var/lib/nke/state.json`. Key state includes:
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

Orchestrators (site-workflow-engine) probe for RPC availability via reflection before calling
apt or InstallCNI workflows. Semver in `GetKubeadmStatus` remains useful for logging only.

## Security Notes

- Vulnerability reporting: see the monorepo root `SECURITY.md`. Node-agent architecture notes: `docs/SECURITY.md`.
- With mTLS enabled (default), the agent uses SPIFFE identities from the workload API (`--spiffe-socket-path`). Use `--disable-mtls` only for local development (plaintext gRPC).
- Reflection is enabled on the same mTLS-protected server; clients need valid SPIFFE credentials to use it in production.
- Encryption provider: secretbox (XSalsa20-Poly1305) for Kubernetes API secrets
