# AGENTS.md — `os-image/`

Reference node OS image (spec + Packer recipe) and the `nke-os-image-check`
connectivity binary.

**Module path:** `github.com/NVIDIA/nke/os-image`

The directory is `os-image/` on purpose. `image.yml` / `image-nvcr.yml` already
publish **container** images; this module is the Ubuntu qcow2 a node boots.

## What to read first

- [SPEC.md](SPEC.md) — bake vs cloud-init, ports, SPIFFE rules
- [README.md](README.md) — how to build the qcow2 and run the checker

## Layout

```text
SPEC.md                   # contract
packer/                   # QEMU golden-image build
files/                    # units + SPIRE template + first-boot script
cmd/nke-os-image-check/   # checker entrypoint
internal/check/           # dial + gRPC reflection / GetKubeadmStatus
```

## Versioning

OS-image releases use component-scoped git tags, same shape as `agent/v*`:

```bash
git tag -s os-image/v0.1.0 -m "os-image v0.1.0"
git push origin os-image/v0.1.0
```

Pushing the tag runs `.github/workflows/os-image-artifact.yml`: Packer bakes the
qcow2, `make boot-smoke` must pass, then the disk is published to
`ghcr.io/nvidia/nke/os-image` and the GitHub Release is created. A failed bake
or smoke test does not publish.

`make` stamps `nke-os-image-check` from:

```bash
git describe --match 'os-image/v[0-9]*' --tags
```

The `os-image/` prefix is stripped for the binary ldflag.

## Commands

```bash
make build              # stamped bin/nke-os-image-check, and asserts the stamp took
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
