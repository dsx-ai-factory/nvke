# Download and Install

`tier0-bootstrap` is distributed as a tarball. A bootstrap installer script automates downloading the correct architecture-specific tarball, extracting the binaries, and installing the systemd service.

## Installing with the Bootstrap Installer Script

The installer script detects the system architecture and downloads the appropriate tarball (x86_64 or aarch64).

> **TODO:** Replace with real release URL once the repo is public and CI publishes release artifacts.

```bash
curl -fsSL https://github.com/NVIDIA/nke/releases/latest/download/bootstrap-installer.sh | sudo bash
```

The installer will:

1. Download the architecture-specific tarball
2. Extract the `tier0-bootstrap` and `nkecli` binaries to the system PATH
3. Install the `tier0-bootstrap` systemd service unit
4. Install the bundled configuration template

After installation, the `tier0-bootstrap` binary is available system-wide.

## Verifying the Installation

```bash
tier0-bootstrap version
```

## Network Requirements

The node must be able to reach the artifact host to download bootstrap artifacts. For nodes provisioned behind NAT, ensure outbound HTTPS to `github.com` and the NGC registry (`nvcr.io`, `helm.ngc.nvidia.com`) is permitted.

## Next Steps

Once the tool is installed on the init node, you can run the bootstrap process. See:

- [Custom Base OS](custom-base-os.md) — cloud-init based automated bootstrap on vanilla Ubuntu
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — manually running the bootstrap CLI on nodes you've already set up
- [Configuration](configuration.md) — full set of available options
