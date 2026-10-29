# DSX qualification tests

**Unattended hardware qualification is not implemented.** Hardware smoke can
check explicitly supplied SSH targets. Without configuration it skips, as do
the other three categories. No test allocates or releases machines.

| Test / CI job | Intended coverage |
| --- | --- |
| `DSXRecipeValidation` | RelEng recipe validation acceptance criteria, section 10. |
| `DSXHardwareSmoke` | Read-only image identity, cloud-init, provisioning NIC, root storage, and driver checks on supplied nodes. NICo provisioning remains pending. |
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

## Draft hardware smoke checks

The expected identity comes from the image build, independently of the node
under test. The opt-in helper below runs **inside the final build guest**, after
its final kernel boot. Supply a unique ID for each build invocation:

```bash
sudo python3 record-qualification-identity.py \
  --build-id "$BUILD_ID" --ubuntu 26.04 --arch arm64 \
  --output /var/tmp/expected-26.04.json
```

The helper is at `packer/scripts/record-qualification-identity.py`, relative to
`os-image/`. It records the configured Ubuntu/architecture, actual build kernel,
and build ID, and writes `/usr/lib/nvke/image-build-id` into the image. Copy its
manifest out with the build artifacts. Packer invocation, artifact digest
binding, and publication of this manifest are **not wired yet**. Never generate
the expected manifest from the hardware node being tested.

Set `DSX_SMOKE_CONFIG` to a JSON file with this structure. Paths are relative to
that file. Include a second target for 24.04 with its own manifest and node:

```json
{
  "machine_type_id": "example-instance-type-id",
  "targets": [
    {
      "manifest": "expected-26.04.json",
      "address": "192.0.2.10",
      "user": "ubuntu",
      "ssh_config": "ssh.conf",
      "provisioning_mac": "02:00:00:00:00:01",
      "nic_driver": "mlx5_core",
      "required_modules": ["mlx5_core", "nvme"],
      "gpu_count": 0
    }
  ]
}
```

Addresses and MACs must come from NICo allocation results; driver expectations
and GPU counts come from the machine profile. The values above are examples.
The SSH config supplies keys, trusted host keys, and any jump-host transport.
Strict host-key checking remains enabled. Nodes need Python 3, iproute2,
cloud-init, and findmnt. The probe installs nothing and creates no cluster.

```bash
DSX_SMOKE_CONFIG=/path/to/targets.json \
  go test -v -count=1 -run '^TestDSXHardwareSmoke$' ./test/qualification
```

Configured runs fail for missing inputs or mismatches. These host checks do not
prove image download integrity, unattended provisioning, cleanup, or hardware
coverage beyond the supplied device expectations. The existing CI workflow
still runs the unconfigured stubs; it does not invoke this opt-in path.

The manual [DSX Machine Image Tests](../../../.github/workflows/dsx-machine-image-tests.yml)
workflow runs one job per category per selected machine type, with both Ubuntu
versions inside each job. Its `machine_types` input accepts a JSON array of NICo
instance type IDs. The default `["configured"]` uses the existing Redstone
instance-type variable. The stubs only record the requested type; they do not
allocate it.
