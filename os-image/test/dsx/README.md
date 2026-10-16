# DSX machine-image hardware tests

This is a draft scaffold for the machine-image portion of NKX-17988. It targets
the existing shared bare-metal pool used by RelEng. Every functional case in
`machine_image_test.go` is an explicit, unimplemented stub; a successful Go test
command here means only that the scaffold compiles. It does not qualify an image.

## Matrix and scope

Each case is listed for Ubuntu **24.04** and **26.04** on **amd64** and **arm64**.
There are 17 cases per combination (68 pending leaf tests). FDE cases use the FDE
image; the other provisioning, upgrade, and reboot cases use the non-FDE image
unless their acceptance statement specifies otherwise. N-1 and N are release
versions within the selected Ubuntu series, not a 24.04-to-26.04 distribution
upgrade.

The cases cover candidate-image provisioning, hardware boot and networking,
platform kernels and devices, TPM attestation, the real FDE provisioning path,
cluster bring-up on FDE, warm reboot and power cycle after join, upgrade/reboot,
failed-image recovery, and cleanup/reuse. Diagnostics and retention across runs
are separately marked SHOULD cases rather than hard acceptance gates.

Exercise Spectrum-X and InfiniBand nodes and each supported GPU/CPU platform as
available. Record missing platform/fabric coverage explicitly. CPU-only success
does not qualify GPU drivers or a fabric, and an unavailable capability must not
be reported as passing. Hardware inventory is a run input, not a site hostname or
machine ID committed to this suite.

KVM-only image-contract, synthetic NIC/disk, and software-TPM scenarios belong to
the separate ARC/KVM work. RelEng's full-rack recipe performance/conformance suite
and pool procurement are outside this scaffold.

## Inspect the stubs

From the repository root:

```bash
cd os-image
go test -v ./test/dsx
```

Expect every functional leaf to report **SKIP: not implemented**. No NICo calls,
machine allocation, image baking, or hardware tests are performed. The existing
module test target discovers this package without a build tag or a new dependency.

## Initial CI jobs

[DSX Machine Image Tests](../../../.github/workflows/dsx-machine-image-tests.yml)
is a manually dispatched advisory workflow, restricted to this repository's
`main` branch. It becomes dispatchable after the workflow lands on the default
branch. Each of the 17 functional tests gets its own matrix job for each of the four
OS/architecture combinations: **68 jobs per dispatch**. Every job selects exactly
one Go subtest and publishes its own result and artifact. Jobs run serially on
the same `arc-amd64-cpu16` runner class as RelEng qualification. ARM64 names the
future remote target; the test driver itself runs on AMD64.

The jobs check the presence of the existing Redstone SSA, Teleport, jump-host,
and allocation settings. They reference the same repository secrets as
`qualify-e2e.yml`; they do not copy secret values or create a new identity.
Credentials are exposed only to the configuration check. No authentication is
attempted, and no credentials are passed to Go tooling or uploaded artifacts.
Each job publishes its selected test result and a summary that explicitly distinguishes
pending cases from executed qualification.

Hardware execution will require wiring those credentials into the actual test
step and adding the Teleport OIDC join/renewal (`id-token: write`), SSA token
exchange, node access, and cleanup. The current configuration-presence check does
not verify token permissions, Teleport's workflow allowlist, or machine reachability.
Redstone provides the ARM64 target; AMD64 hardware still needs a suitable site
allocation rather than an architecture flag on Redstone.

The subtests can stay in one Go file: `go test -run` selects a single named
case independently. Adding or renaming a functional case also requires updating
the workflow's `test` matrix. The job rejects selectors that produce anything other
than one functional result.

## Implementation boundaries

The eventual job will use the existing ARC runners, shared pool, and approved
Teleport and NICo access. It must coordinate with RelEng's allocation/lock before
creating or retaining machines. Host provisioning, SSH, credentials, artifacts,
and test assertions remain unimplemented in this scaffold.

Use the provisioning workflow from [PR #1440](https://github.com/dsx-ai-factory/nvke/pull/1440)
as a reference for the access pattern. This suite does not invoke its
site-controller tests or count their results as its own qualification evidence.
Candidate-image selection and recovery must be verified for the chosen site;
reusing credentials does not establish that image-registration, reboot, or FDE
API capabilities are available.

Before enabling hardware execution, settle:

- Per-run candidate-image registration or an equivalent image-URL path, plus
  cleanup and known-good recovery.
- API reboot and power cycle without reimage, API console-log availability, and
  pre-bootstrap diagnostics access.
- Physical TPM availability, SPIRE attestation configuration, and ownership of
  the FDE `luks.key` handoff through NICo iPXE.
- Shared-pool scheduling, explicit retention, cleanup on failure, and sufficient
  resources for the cluster-level cases.
- Pinned N-1/N inputs, hardware/firmware identity, and credentials supplied at
  runtime through the approved CI configuration.

Replace each stub with its actual setup, operation, assertions, and cleanup as
the corresponding capability is implemented. Promotion of this scaffold to an
acceptance lane must detect any remaining mandatory skipped cases; green compile
checks and missing hardware must never be substituted for qualification.
