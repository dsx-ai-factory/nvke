> **Note:** This component is being migrated from internal repositories. Script names, Makefile targets, and artifact paths will be finalized during migration. The information here reflects the intended state.

# build

Scripts for building and publishing NKE component artifacts — the CLI binary, nke-agent image, Cloud Workflow Engine image, and node images — into an operator-controlled registry.

These scripts are the operator's entry point into the NKD build pipeline. They wrap the component-specific build tooling and produce the artifacts NKE consumes.

## Scripts

| Script | What it builds |
|--------|---------------|
| `cli.sh` | `nkectl` binary (multi-arch) |
| `node-agent.sh` | `nke-agent` Debian package and container image |
| `controller.sh` | Cloud Workflow Engine container image |
| `node-image.sh` | Node OS image (QCOW2) from an NKD recipe |

> **TODO:** Build tooling format (Makefile, Bazel, shell scripts, or a combination) is not yet decided. Finalize during migration and document invocation syntax here.

## Kubernetes packages

NKE node images and pre-provisioned node setup require version-pinned Kubernetes Debian packages (`kubeadm`, `kubelet`, `kubectl`, `containerd`). These are assembled by the package builder, which produces a versioned apt repository operators can mirror into their own infrastructure.

Supported Kubernetes versions and their package collections are listed in `pkg-builder/collections/`.

## Related docs

- [Architecture](../docs/architecture.md)
- [Node OS Image](../os-image/README.md)
- [node-agent](../agent/README.md)
- [Pre-Provisioned Nodes](../docs/site-bootstrap/pre-provisioned-nodes.md)
