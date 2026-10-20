# AGENTS.md — `validate/`

The node-image validator: an offline CLI that checks a built NVKE node image
against its release evidence and a versioned check registry, plus the
verification library the management plane uses to check the resulting verdict.

**Module path:** `github.com/dsx-ai-factory/nvke/validate`

## What this is

Operators build node images with the NKD pipeline and run
`nvke-image-validate` against the result before registering it. The tool is a
batch CLI, not a controller and not a webhook: validation happens once, offline,
in the build pipeline; the management plane only ever re-checks the signed
verdict (milliseconds), never the image.

Three inspection tiers, cheapest first; every check declares its tier in
`checks.yaml`:

- **static** — no image open: digests, release-checksums membership (with the
  attestation over them verified offline via sigstore), identity, target BOM
  vs release SBOM.
- **guestfs** — read-only filesystem facts via libguestfs, following
  `os-image/packer/scripts/extract-image-sbom.sh`'s conventions (direct
  backend, LUKS passphrase delivery to the daemon).
- **boot** — behavioral assertions from inside a throwaway QEMU boot
  (`-snapshot`; the artifact's bytes are never written).

## Layout

```text
checks.yaml                  # the versioned check registry: what + why, never how
cmd/nvke-image-validate/     # CLI entrypoint; exit 0 pass / 1 fail / 2 error
cmd/nvke-verdict/            # reference verdict verifier (what a gate runs)
pkg/bundle/                  # release evidence bundle: loader + offline sigstore
                             # verification + SBOM extraction
pkg/checks/                  # registry loader + engine; implementations per tier
                             # in static/, imagefs/, boot/
pkg/inspect/guestfs/         # libguestfs session (read-only, LUKS-aware)
pkg/inspect/qemu/            # throwaway QEMU boot + SSH probe channel
pkg/report/                  # report.json schema v1, human renderer, exit policy
pkg/verify/                  # verdict DSSE sign/verify; self-contained so
                             # consumers vendor only this package
docs/decisions.md            # design decisions and their reasons
hack/                        # fixture generator + containerized Linux test runners
```

## Constraints

- **Offline is a contract.** No package in this module may open a network
  connection; air-gapped operator pipelines are the primary environment. Trust
  roots travel in the evidence bundle. Tests run hermetically from generated
  or vendored fixtures.
- **Exit codes are API**: 0 pass, 1 blocking-check fail, 2 could-not-evaluate
  (usage errors included). Error always outranks fail — "could not evaluate"
  must never read as "evaluated, non-compliant".
- **checks.yaml entries say what and why, never how.** Implementations are
  bound by ID in the engine; an entry with no implementation reports an error,
  not a silent pass. Every entry needs `verifies` and `why`; the registry
  version travels into every report.
- **Both unit-name generations are valid.** The nke→nvke rename (#1548, #1565)
  lands after os-image v0.2.0; name-sensitive checks resolve which generation
  the image carries and must keep passing on both.
- `--bundle` verifies the evidence (attestation, pinned trusted root, signer
  identity) before any check runs; `--checksums` is digest-only mode and
  reports the unverified evidence as a visible skip, never a pass.

## Commands

```bash
make build      # stamped bin/nvke-image-validate
make compile    # type-check every package, no binary
make test       # unit tests (hermetic, no network, no KVM)
make lint       # golangci-lint at the repository pin
make license    # SPDX headers for new files
```

Integration suites need a Linux kernel and run in containers — any Docker
daemon backed by a Linux kernel works:

```bash
hack/test-guestfs-container.sh   # guestfs tier against a generated LUKS+LVM fixture
hack/test-boot-container.sh      # boot tier against a cached Ubuntu cloud image
```

`hack/cross-verify-cosign.sh` (any OS, needs cosign v3+) proves emitted
verdicts verify with the sigstore ecosystem's own tooling.

## Verification — run before submitting a pull request

```bash
make fmt
make vet
make lint
make test
```

From the repository root after adding/moving test files or packages:

```bash
make coverage-check
```
