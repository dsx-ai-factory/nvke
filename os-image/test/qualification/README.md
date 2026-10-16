# DSX qualification tests

This is a draft scaffold for the four qualification categories named in NKX-17988.
It targets the existing shared bare-metal pool used by RelEng. Every Ubuntu-version
subtest is explicitly unimplemented and skipped. A successful Go command only
checks the scaffold; it does not qualify an image.

## Tests and scope

| Test / CI job | Intended acceptance |
| --- | --- |
| `DSXRecipeValidation` | RelEng recipe validation acceptance criteria in section 10 of the requirements document linked from the epic. |
| `DSXHardwareSmoke` | Provision a candidate image through NICo and check image identity, boot, cloud-init networking, and applicable hardware drivers. |
| `DSXFDEEndToEnd` | Verify the iPXE key handoff, real TPM attestation and binding, encrypted root, and FDE-backed cluster/workload health. |
| `DSXUpgradeAndReboot` | Verify ordinary and FDE node reboot/power cycle without reimage, N-1 to N upgrade, preserved state, unattended FDE unseal, and recovered health. |

Each test contains Ubuntu **24.04** and **26.04** subtests. There are **four functional tests**. CI runs one job per test per selected
machine type, with two pending Ubuntu-version results per job. Architectures and
fabrics depend on the selected pool hardware.
Record actual hardware coverage when implementing the tests. Missing hardware
capabilities must not be counted as passing.

The RelEng stub reserves an integration point for its acceptance suite. It does
not duplicate that suite or implement the section 10 criteria. The remaining
three stubs cover the additional OS-image hardware qualification. Provisioning,
cleanup/recovery, pool coordination, and failure diagnostics belong to each test's
shared lifecycle support rather than separate top-level jobs. KVM-only scenarios
and pool procurement are outside this scaffold.

## Inspect the stubs

From the repository root:

```bash
cd os-image
go test -v ./test/qualification
# Select one functional test, including both Ubuntu versions:
go test -v -run '^TestDSXHardwareSmoke$' ./test/qualification
```

Expect every Ubuntu-version subtest to report **SKIP: not implemented**. No NICo
calls, machine allocation, image baking, or hardware tests are performed. The
existing module test target discovers this package without a build tag. All four functions can stay in one Go file and run independently.

## Initial CI jobs

[DSX Machine Image Tests](../../../.github/workflows/dsx-machine-image-tests.yml)
is a manually dispatched advisory workflow, restricted to this repository's
`main` branch. It becomes dispatchable after landing on the default branch.
The test and machine-type matrix jobs each select one functional test, run both Ubuntu versions,
and publish a separate result artifact and pending-coverage summary. Jobs run
serially on the same `arc-amd64-cpu16` runner class as RelEng qualification.
The runner drives remote hardware; its architecture does not select the target.

The jobs check the presence of the existing Redstone SSA, Teleport, jump-host,
and allocation settings. They reference the same repository secrets as
`qualify-e2e.yml`; they do not copy secret values or create an identity.
Credentials are exposed only to the configuration check. No authentication is
attempted, and no credentials are passed to Go tooling or uploaded artifacts.
The result check requires two Ubuntu-version results for the selected test.

The `machine_types` dispatch input is a JSON array of NICo instance type IDs
available in the configured Redstone pool. Its default, `["configured"]`, uses
`DSX_REDSTONE_DEV1_DH2_INSTANCE_TYPE_ID`. Supply multiple IDs to run all four tests
against each type: two types produce eight jobs. Job names, summaries, and test
logs identify the requested type, and each job has a distinct artifact.
`DSX_MACHINE_TYPE_ID` passes the selected ID to the shared test harness; the stubs
record it, and future provisioning must use it. For a local invocation:

```bash
DSX_MACHINE_TYPE_ID=example-instance-type-id go test -v ./test/qualification
```

Machine type selection uses the existing pool's credentials and network settings.
Testing another site will also require that site's allocation configuration. A
new type does not require new test functions or a change to their Go file.

## Shared qualification infrastructure

These hardware tests belong to the existing `os-image` Go module. Follow the
repository E2E conventions when implementing provisioning, hardware selection,
and qualification reporting. The controller suite's provisioning helpers are
under `controller/test/e2e/internal`, so this package cannot import them directly.
SSA exchange is available through the new public `controller/test/e2e/dsxauth`
adapter: `dsxauth.MintToken(ctx, datahall)` delegates to the existing implementation
without modifying it. Each call exchanges credentials; it does not maintain a
shared token cache. Never log or upload the returned token.

`auth_test.go` verifies the adapter from this module against a local mock issuer,
including per-datahall credential selection and rejection. It uses no live
credentials and is separate from the four pending hardware tests. The OS-image
module uses a local dependency on the controller E2E module for this adapter;
standalone builds resolve it through `go.mod` replacements, as they do the agent.
Broader provisioning helpers still require an explicit public integration boundary.

## Implementation boundaries

Hardware execution still needs Teleport OIDC join/renewal (`id-token: write`),
SSA token exchange, node access, candidate-image selection, and cleanup wired
into the test execution. Configuration presence does not prove token permissions,
Teleport workflow authorization, or machine reachability. Redstone provides an
ARM64 target; any additional architecture needs suitable pool hardware.

Use [PR #1440](https://github.com/dsx-ai-factory/nvke/pull/1440) as an access-pattern
reference. Coordinate allocation with RelEng before creating or retaining machines.
Resolve the epic's open NICo questions about candidate-image registration, reboot,
console logs, physical TPM/attestation, and the FDE key handoff before implementing
the corresponding checks. N-1 and N identify releases within an Ubuntu series,
not a 24.04-to-26.04 distribution upgrade.

Replace the stubs with setup, operations, assertions, and cleanup. Promotion to an
acceptance lane must detect remaining mandatory skips; green scaffold checks do
not satisfy the epic's definition of done.
