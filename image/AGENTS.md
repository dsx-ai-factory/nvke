# AGENTS.md — `image/`

Reference node image (spec + Packer recipe) and the `nke-image-check`
connectivity binary.

**Module path:** `github.com/NVIDIA/nke/image`

## What to read first

- [SPEC.md](SPEC.md) — bake vs cloud-init, ports, SPIFFE rules
- [README.md](README.md) — how to build the qcow2 and run the checker

## Layout

```text
SPEC.md                 # contract
packer/                 # QEMU golden-image build
files/                  # units + SPIRE template + first-boot script
cmd/nke-image-check/    # checker entrypoint
internal/check/         # dial + gRPC reflection / GetKubeadmStatus
```

## Commands

```bash
make build              # stamped bin/nke-image-check, and asserts the stamp took
make compile            # type-check every package, no binary
make test
make lint
```

From the repository root, `make build` links the same binary into the
repository's `bin/`, and `make version-check` runs it with `--version`.

## Constraints

- Do not bake SPIRE server address, trust domain, trust bundle, or join token.
- Do not default `nke-agent` to `--disable-mtls`.
- Pin SPIRE (version + sha256) in `packer/scripts/fetch-artifacts.sh`; do not
  float `latest` for SPIRE.
- `nke-agent` version may be `edge` or `vX.Y.Z`. Packer pulls the `.deb` from
  `ghcr.io/nvidia/nke/agent-deb` via oras, and falls back to the OCI binary at
  `ghcr.io/nvidia/nke/agent` when that tag has no package.
- Checker is a SPIFFE **client**. Do not embed a SPIRE server in the binary.
- New files need the Apache-2.0 SPDX header (`make license`). Cloud-init
  user-data cannot: `#cloud-config` must be line 1, so examples live in
  README/SPEC rather than `*.yaml`.
