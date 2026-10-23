# DSX qualification tests

**Hardware tests are stubs only.** Each category has Ubuntu 24.04 and 26.04
subtests that explicitly skip as unimplemented. They do not authenticate to DSX,
provision machines, or qualify an image.

| Test / CI job | Intended coverage |
| --- | --- |
| `DSXRecipeValidation` | RelEng recipe validation acceptance criteria, section 10. |
| `DSXHardwareSmoke` | Candidate-image provisioning, boot, cloud-init networking, and hardware drivers. |
| `DSXFDEEndToEnd` | FDE key delivery, real TPM attestation and binding, encrypted root, and cluster/workload health. |
| `DSXUpgradeAndReboot` | Ordinary and FDE node upgrade/reboot, preserved state, unattended unseal, and recovered health. |

## Run the stubs

From the repository root:

```bash
cd os-image
go test -v ./test/qualification
# Select one category, including both Ubuntu versions:
go test -v -run '^TestDSXHardwareSmoke$' ./test/qualification
# Record a requested machine type in the stub output:
DSX_MACHINE_TYPE_ID=example-instance-type-id go test -v -run '^TestDSXHardwareSmoke$' ./test/qualification
```

Expect the hardware subtests to report **SKIP: not implemented**.

The manual [DSX Machine Image Tests](../../../.github/workflows/dsx-machine-image-tests.yml)
workflow runs one job per category per selected machine type, with both Ubuntu
versions inside each job. Its `machine_types` input accepts a JSON array of NICo
instance type IDs. The default `["configured"]` uses the existing Redstone
instance-type variable. The stubs only record the requested type; they do not
allocate it.
