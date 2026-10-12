# Proposal: Bring Your Own OS (BYO-OS)

> Status: **proposal** — for discussion. Describes a shift from NVKE-owned OS
> image construction to an operator-owned node readiness contract, with
> cluster software installed afterward by the NVKE controller through `nke-agent`.

---

## 1. Summary

Today NVKE tightly couples cluster provisioning to how the node OS is built and
bootstrapped: we publish custom qcow images with packages pre-installed, and/or
drive package install and agent bring-up through cloud-init at provision time
(via OS profiles under `profiles/osProfiles/`). That often includes kubeadm,
CNI image packages, Teleport, and more — baked into the image or first-boot
user-data.

This proposal reframes that relationship into two clear phases:

1. **BYO-OS (operator-owned):** the node meets a minimal readiness contract
   (Ubuntu 24.04, NVKE apt repo, `nke-agent`, `spire-agent`, TPM). *How* that
   happens is out of scope for NVKE.
2. **Post-provision bootstrap (NVKE-owned):** once the agent is reachable over
   SPIFFE mTLS, the controller drives the rest through `nke-agent` — kubeadm packages,
   CNI, Teleport, cluster identity config, and lifecycle operations.

Operators may satisfy phase 1 with a Packer-built qcow, cloud-init at first
boot, Ansible after instance create, a golden AMI, or any other mechanism.
That choice is **out of scope for the NVKE team**.

---

## 2. Motivation

### Current model

The controller provisions nodes by selecting an **OS profile** that typically includes:

- An iPXE / image URL pointing at an NVKE-built Ubuntu qcow
- A cloud-init `userData` template that installs packages from the NVKE apt repo
  (`nke-agent`, `spire-agent`, kubeadm distro packages, CNI image packages,
  Teleport, …), writes SPIRE / Teleport config, enables systemd units, and
  signals readiness (e.g. `phone_home`, `/run/cloud-init.done`)

That path works, but it makes the NVKE team responsible for:

- Image build, publish, and version churn for every supported Ubuntu / arch /
  FDE variant
- Encoding bootstrap policy in cloud-init templates that operators cannot easily
  replace
- Shipping cluster software (kubeadm, CNI, Teleport) in the base image / first
  boot path, which couples OS provenance to Kubernetes version and CNI choice
- Debugging provisioning failures that are really OS / packaging / site-local
  configuration problems

### Desired model

Treat the node like any other platform dependency: **publish a minimal readiness
contract, validate it, then configure the node as an NVKE bootstrap step.**

| Phase | Owner | Responsibility |
|-------|--------|----------------|
| Bring OS / agents online | **Operator** | Satisfy the BYO-OS contract by any means |
| Cluster bootstrap & lifecycle | **NVKE (controller → nke-agent)** | Install kubeadm / CNI / Teleport / etc., inject identity, run kubeadm, manage the node |

NVKE continues to own:

- The agent gRPC API and its security model (SPIFFE mTLS)
- Post-provision package install and cluster orchestration via the agent
- Publishing apt packages and documenting required versions / repos
- Optional reference images or example bootstrap snippets (non-normative)

NVKE does **not** own:

- How Ubuntu is installed or hardened
- Whether the minimal agent packages are baked into an image or installed
  post-create
- Site-local tooling (Packer, Ansible, Ignition, custom CI, …)

---

## 3. Goals and non-goals

### Goals

1. Define a clear, minimal **node readiness contract** that the controller can check before
   NVKE bootstrap begins.
2. Make OS construction **operator-owned**; keep cluster software install as an
   **NVKE-owned** step after the agent is up.
3. Stop putting kubeadm, CNI, Teleport, and similar cluster/ops packages in the
   base image or cloud-init install list as the normative path.
4. Keep a supported path for today’s NVKE qcow + cloud-init profiles as one
   *implementation* of phase 1, not the only one.
5. Reduce NVKE image and cloud-init surface area over time.

### Non-goals

- Supporting arbitrary Linux distributions in v1 (Ubuntu 24.04 only).
- Replacing SPIRE TPM attestation or mTLS with a weaker auth model.
- NVKE owning or blessing a particular image builder / config management tool.
- Asking operators to pre-install kubeadm, CNI, Teleport, or other cluster
  software in their golden images.

---

## 4. Node readiness contract (phase 1)

A node is **BYO-OS ready** when all of the following are true. How they became
true is irrelevant to NVKE.

| # | Requirement | Notes |
|---|-------------|--------|
| 1 | **Ubuntu 24.04** | `amd64` / `arm64` as required by the node pool. Kernel and userspace must be a supported 24.04 LTS baseline. |
| 2 | **NVKE apt repository configured** | Sources and signing keyrings present so later `apt-get install` (from the agent) can resolve NVKE packages. Exact repo URL / suite is environment-specific and documented with the artifact publish pipeline. |
| 3 | **`nke-agent` installed and running** | systemd unit active; gRPC listening on the configured secure port (default `15005`); production deployments must not disable mTLS. |
| 4 | **`spire-agent` installed and running** | Workload API available to `nke-agent`. TPM node attestor plugin present where TPM attestation is required. |
| 5 | **TPM utilities and enablement** | Hardware or firmware TPM 2.0 visible to the guest/host as applicable; userspace tools needed for attestation available (e.g. `tpm2-tools`). See §8 for FDE caveats. |

### Explicitly **not** part of the BYO-OS contract

These must **not** be required in the operator’s base image. They are installed
and configured afterward by NVKE (controller → `nke-agent`):

| Component | Why post-provision |
|-----------|-------------------|
| **kubeadm / Kubernetes distro packages** | Versioned with the cluster, not the OS image. Coupling them to golden images has been a long-standing pain point. |
| **CNI packages / chart images** (OVN, Calico, …) | Cluster network choice and version belong to cluster config, not the OS. |
| **Teleport** (and similar ops agents) | Site/env policy applied by NVKE bootstrap once the agent can install packages and write config securely. |
| Other cluster-scoped packages | Anything whose version or presence depends on the cluster profile rather than “can this node speak to the controller?” |

### Phase boundary

```text
┌─────────────────────────────────────────────────────────────────┐
│              Phase 1 — operator-owned (BYO-OS)                  │
│                                                                 │
│  Packer/qcow  ──┐                                               │
│  cloud-init   ──┼──► Ubuntu 24.04 + apt repo + nke-agent        │
│  Ansible      ──┤      + spire-agent + TPM ready                │
│  manual / CI  ──┘                                               │
└───────────────────────────────┬─────────────────────────────────┘
                                │ contract met; agent reachable
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│     Phase 2 — NVKE-owned (controller → nke-agent bootstrap)      │
│                                                                 │
│  • SPIRE trust material / agent config (if not already present) │
│  • Install kubeadm distro packages                              │
│  • Install CNI packages                                         │
│  • Install/configure Teleport (when enabled for the site)       │
│  • kubeadm init/join, CNI deploy, encryption, …                 │
└─────────────────────────────────────────────────────────────────┘
```

**Operator:** base OS + repos + `nke-agent` + `spire-agent` + TPM enablement.

**NVKE:** everything from “agent is up” onward — package installs that today often
live in cloud-init `runcmd`, plus cluster lifecycle.

Operators may still apply extra site config themselves if they wish; the controller only
requires that the node can attest and speak mTLS so NVKE bootstrap can proceed.

---

## 5. Operator implementation patterns (illustrative)

All of the following are valid for **phase 1**. NVKE does not prefer one.

| Pattern | Sketch |
|---------|--------|
| Pre-built image | Packer (or equivalent) bakes Ubuntu 24.04, configures the NVKE apt repo, installs `nke-agent` / `spire-agent` / TPM tools. **Does not** bake kubeadm, CNI, or Teleport. |
| Provision-time install | Stock Ubuntu cloud image; operator installs only the contract packages after create. |
| Post-create config management | Minimal image + Ansible (or similar) converges the contract packages/services before controller bootstrap. |

---

## 6. Implications for the controller and OS profiles

### Today

OS profiles are the primary vehicle for both **image selection** and **bootstrap
policy** (iPXE image URL + cloud-init that installs kubeadm, CNI, Teleport,
agents, etc.).

### Under BYO-OS

OS profiles (or a successor “node environment” object) should evolve toward:

1. **Pointer to an operator-supplied image / launch template** (or “already
   running instance” registration), not an NVKE-owned qcow URL packed with
   cluster software.
2. **Thin first-boot overlays** only where unavoidable (hostname, network,
   enough SPIRE config for mTLS to come up) — not a full package bootstrap
   script for kubeadm/CNI/Teleport.
3. **Post-agent bootstrap activities** in controller workflows: `ConfigureAptRepo` (if
   needed), `InstallPackages` for kubeadm / CNI / Teleport, write Teleport /
   remaining SPIRE config, then existing kubeadm / `InstallCNI` flows.
4. **Readiness gates** that assert the phase-1 contract — agent reachability,
   `ExecuteNodePreflight`, SPIRE SVID availability — before phase-2 package
   installs begin.

NVKE may continue to ship **reference** OS profiles and example user-data for
labs and nico-vms. Those are convenience implementations of phase 1, not
requirements. Reference images should prefer the thin contract over “kitchen
sink” package lists.

### Suggested readiness checks (controller)

Before phase-2 bootstrap (kubeadm packages, CNI, Teleport, init/join):

1. TCP / gRPC reachability to `nke-agent` (`NodeAgentReady`)
2. Optional `ExecuteNodePreflight` (and/or operator-documented sentinels)
3. mTLS handshake success (SPIRE-issued SVID present for both sides)

Failure mode: leave the node in a clear “OS contract not met” condition rather
than proceeding into flaky kubeadm failures.

---

## 7. What NVKE still publishes

BYO-OS does not mean operators invent packages from scratch.

| Artifact | Owner | Role under BYO-OS |
|----------|--------|-------------------|
| `nke-agent` `.deb` | NVKE | **Phase 1** — required on the node before bootstrap |
| `spire-agent` (NVKE-packaged / TPM-enabled build as applicable) | NVKE / platform | **Phase 1** — required |
| NVKE apt repo metadata + signing keys | NVKE | **Phase 1** — repo must be configured so phase 2 can install |
| kubeadm / Kubernetes distro packages | NVKE | **Phase 2** — installed via controller → agent after readiness |
| CNI packages / chart images | NVKE | **Phase 2** — installed via controller → agent after readiness |
| Teleport (when enabled) | NVKE / site policy | **Phase 2** — installed/configured via controller → agent |
| Reference qcow / cloud-init examples | NVKE (optional) | Non-normative phase-1 helpers |
| Production golden images for a given site | **Operator** | Implements phase 1 only |

---

## 8. Caveat: full-disk encryption (FDE)

FDE does **not** fit cleanly into “operator brings any Ubuntu and we apt-install
the rest.” Disk layout, LUKS, unlock policy, and often the image itself are
decided **before** `nke-agent` is reachable. Treat FDE as an explicit overlay on
BYO-OS, not as silent part of the minimal contract.

### Why it is special

- Encryption and partitioning happen at image-write / first-boot time (today:
  qcow-imager, LUKS key material, LVM layouts in OS profiles).
- Unlock may depend on TPM sealing, clevis/tang, or passphrase injection that
  must work *before* normal multi-user bootstrap.
- An operator “stock cloud image” without the expected layout will not become an
  FDE node just because phase-2 package installs succeed.

### Proposed stance

| Mode | Expectation |
|------|-------------|
| **Non-FDE BYO-OS** | Phase-1 contract only. No NVKE requirement on LUKS/LVM layout. |
| **FDE-enabled pools** | Operator (or NVKE reference image) must supply an image / boot path that already implements the required disk encryption layout and unlock story. TPM enablement in phase 1 remains necessary but is not sufficient by itself. |

NVKE should document, for FDE pools:

1. Required disk layout / unlock method (as applicable to the profile).
2. Whether the controller still participates in key injection or only consumes an
   already-encrypted disk.
3. Which TPM / clevis packages must be present in phase 1 vs installed later
   (prefer phase 1 for anything needed to unlock or attest at boot).

FDE reference images may remain an NVKE-published artifact longer than generic
BYO-OS images, because the encryption story is harder for operators to
reimplement correctly. That does **not** put kubeadm/CNI/Teleport back into
those images — FDE images should still aim for the thin phase-1 package set.

### Open FDE questions

- Is FDE always operator-supplied image + documented layout, or does NVKE keep
  shipping FDE qcow as the supported path indefinitely?
- How much LUKS/TPM unlock automation stays in iPXE / imager vs moves behind
  the agent after first boot?
- Clevis/tang vs TPM-only unlock: which are first-class for BYO-OS FDE?

---

## 9. Security considerations

- **Trust boundary unchanged:** a reachable `nke-agent` with a valid SPIFFE
  identity is still root-equivalent for that node. BYO-OS must not weaken mTLS
  or TPM attestation requirements.
- **Supply chain:** operators who bake images are responsible for verifying apt
  repo keys and phase-1 package provenance. Phase-2 packages are installed by
  NVKE from the same signed apt repo after the agent is up.
- **TPM:** sites that rely on TPM node attestation (or FDE unlock) must ensure
  the guest has a usable TPM; software TPM in lab (e.g. nico-vms swtpm) is fine
  for dev, not a production substitute unless explicitly accepted.
- **Cluster secrets:** trust bundles, Teleport join tokens, and similar must not
  be baked into shared golden images reused across trust domains; prefer
  phase-2 injection via controller → agent (or an equivalent secure channel).

See also `agent/docs/SECURITY.md`.

---

## 10. Migration sketch

1. **Document** this contract (this proposal → accepted design).
2. **Move** kubeadm / CNI / Teleport installs out of normative cloud-init
   `runcmd` into controller → `nke-agent` bootstrap activities.
3. **Validate** phase 1 in controller provisioning (explicit conditions / preflight)
   without removing existing OS profiles.
4. **Thin** reference images and OS profiles to the phase-1 package set;
   keep FDE reference images as needed (§8).
5. **Offer** BYO-OS as an opt-in node pool / profile mode for early operators.
6. **Deprecate** NVKE ownership of site-specific non-FDE golden image pipelines
   once reference + contract are sufficient; keep nico-vms / CI images as test
   fixtures.

No big-bang cutover is required. The current qcow + cloud-init path remains a
valid way to satisfy phase 1 while phase-2 work lands.

---

## 11. Open questions

1. **Version pins:** does phase 1 require exact package versions
   (`nke-agent=…`) or only “≥ N / from channel X”?
2. **SPIRE config at first boot:** minimum overlay so mTLS works before phase 2
   (trust bundle / server address) — cloud-init vs agent-first chicken-and-egg?
3. **Registration API:** do operators hand the controller an image ID only, or also a
   “bootstrap complete” webhook / phone_home equivalent when not using
   cloud-init?
4. **Ubuntu 22.04:** sunset timeline for any remaining 22.04 profiles once
   24.04-only is the contract?
5. **FDE:** see open questions in §8.

---

## 12. Decision request

Approve the direction that:

1. NVKE specifies **Ubuntu 24.04 + NVKE apt repo + `nke-agent` + `spire-agent` +
   TPM enablement** as the phase-1 node readiness contract.
2. **How** phase 1 is met is **out of scope for NVKE** and owned by the operator.
3. **kubeadm, CNI, Teleport, and similar** are **not** base-image requirements;
   they are installed and configured by **the controller → nke-agent** after provisioning.
4. **FDE** is a documented caveat (§8): encryption layout is not “just another
   apt package”; FDE pools need an explicit image/boot contract on top of BYO-OS.
5. The controller evolves OS profiles toward thin phase-1 bring-up + agent-driven phase-2
   bootstrap, with current NVKE images retained as reference implementations.

Once agreed, follow-ups are: tighten the contract into a versioned checklist,
land phase-2 install activities for packages currently in cloud-init, add controller
readiness condition wording, document the FDE overlay, and publish a short
operator guide with non-normative Packer / Ansible / cloud-init examples for
phase 1 only.
