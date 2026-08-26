> **Note:** This component is being migrated from internal repositories. Recipe formats, Packer variable names, Ansible role structure, and build targets will be finalized during migration. The information here reflects the intended state.

# node-images

NKD node image build pipeline. Produces bootable OS images (QCOW2) for NKE nodes from validated [AICR](https://github.com/NVIDIA/aicr) recipes that pin the Kubernetes version, GPU driver, and runtime operator stack.

This is the build side of the NKD (NVIDIA Kubernetes Definition) — the output of this pipeline is what operators boot their NKE nodes from.

## What it does

- Builds Ubuntu 24.04 QCOW2 images for NKE nodes in x86_64 and aarch64 variants
- Supports plain and LVM/LUKS-encrypted (FDE) image variants
- Applies the NKD recipe: version-pinned Kubernetes packages, GPU/network operator configuration, kernel settings, CIS hardening
- Emits a CycloneDX SBOM for every build
- Runs CVE scanning against produced images
- Supports multi-site parallel deployment of built images

## Language and tech

Packer HCL (image build), Ansible (OS configuration roles), Go (deploy tooling, image utilities), shell scripts.

## Structure

```
components/         Base Ansible roles applied to every image
  node-agent/       Installs and configures nke-agent
  kube/             Kubernetes packages (kubeadm, kubelet, kubectl)
  cri/              Container runtime (containerd)
  cni/              CNI plugin installation
  luks_tpm/         Full-disk encryption with TPM unsealing
  grub/             Boot configuration
  build-metadata/   SBOM and image metadata embedding

recipes/            Validated NKD recipes (AICR manifests)
  nke-26-10-ubuntu-2404-amd64.yaml
  nke-26-10-ubuntu-2404-arm64.yaml
  ...
```

> **TODO:** Build tooling format (Makefile, Bazel, or shell scripts) is not yet decided. Finalize during migration. The pipeline takes a recipe and a target registry as inputs and produces a QCOW2 image and matching SBOM — document invocation syntax once settled.

## Recipes

A recipe is an [AICR](https://github.com/NVIDIA/aicr) manifest that declares the exact component versions for a validated NKE node image. Operators fork or reference a recipe and run the build pipeline to produce images in their own registry.

NVIDIA validates recipes against the NKE management plane before publishing. Operators pick up patched recipes as CVEs land and re-run the pipeline — the node image is rebuilt rather than patched in place.

## Related docs

- [Architecture](../docs/architecture.md) — NKD in context
- [Pre-Provisioned Nodes](../docs/site-bootstrap/pre-provisioned-nodes.md) — using node images for bootstrap
