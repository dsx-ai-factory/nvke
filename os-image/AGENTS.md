# AGENTS.md — `os-image/`

Reference node OS image (spec + Packer recipe) and the `nke-os-image-check`
connectivity binary.

**Module path:** `github.com/dsx-ai-factory/nke/os-image`

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
`ghcr.io/dsx-ai-factory/nke/os-image` and the GitHub Release is created. A failed bake
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
- Pin SPIRE and the TPM attestor plugin (version + sha256): `SPIRE_VERSION` /
  `TPM_PLUGIN_VERSION` in `make/tool-versions.mk`, which owns them for the
  whole repository, and digests in `packages/spire-agent/lock/`. Do not float
  `latest`, and regenerate the lock with `make lock` rather than hand-editing
  it. The DSX IMDS node attestor is pinned in that same lock. Its S3 objects
  are unversioned and have no checksum sidecar, so the lock digest is the only
  pin. `TPM_PLUGIN_VERSION` also selects the server-side attestor and the CAPD
  bundles; `controller/hack/ci/prepare-capd-bundles.sh` and
  `controller/hack/capd-kindest-node/build-image.sh` read the pin rather than
  restating it, and `make tool-version-check` fails if anything declares it a
  second time. Three consumers embed the version inside an image tag or an apt
  package spec and so cannot be checked — `make/tool-versions.mk` lists them
  above the pin, and they move by hand in the same commit.
- Keep the generic image target's TCG development path. `image-native` requires
  usable native KVM; CI uses matching Linux KVM runners for both architectures.
  Package builds execute Linux kubeadm; matching Linux hosts build locally,
  while Linux and Apple Silicon macOS hosts otherwise use the separate NixOS
  package-builder VM (`package-builder.sh` rejects Intel macOS). The shared
  QEMU helper also supports HVF for boot tools on macOS.
- Preserve `packages/spire-agent/` installation and the current package builders.
  `flake.nix` and `nix/` define the NixOS package-builder fallback; see `nix/README.md` for the integration status and Ubuntu-linkage checks.
- Run `make test-qemu` for QEMU selection and build-entrypoint changes; the
  module's `make test` also runs these tests.
- `nke-agent` is built as a Debian package from this checkout's `agent/` source
  at bake time (`make -C agent deb-<arch>` in `fetch-artifacts.sh`) and installed
  by dpkg with `NKE_AGENT_SKIP_START=1`. No prebuilt artifact is pulled. Its
  version is the checkout's `git describe --match 'agent/v*'`. Keep the image's
  SPIRE ordering in `files/nke-agent-spire.conf`, separate from package-owned units.
- Checker is a SPIFFE **client**. Do not embed a SPIRE server in the binary.
- New files need the Apache-2.0 SPDX header (`make license`). Cloud-init
  user-data cannot: `#cloud-config` must be line 1, so examples live in
  README/SPEC rather than `*.yaml`.
