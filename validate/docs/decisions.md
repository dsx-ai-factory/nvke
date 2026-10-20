<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# validate/ — design decisions

Dated, append-only. Each entry records a decision this module acts on and the
reason, so a reviewer (or a future change) does not have to re-derive it.

## 2026-10 — A batch CLI, not an admission webhook or controller

Validation happens once, offline, where the image is built. A validating
webhook would put image-sized work on the cluster's request path and assume
network reachability that air-gapped operator sites do not have. The management
plane's enforcement input is the signed verdict, re-checked in-process in
milliseconds — never the image.

## 2026-10 — Offline is a contract, not a mode

No package in this module opens a network connection. The sigstore trusted
root is pinned and travels in the evidence bundle; verification uses observer
timestamps, not Rekor lookups. This is also why the module avoids any client
SDK that assumes registry or transparency-log reachability.

## 2026-10 — Checks are data; implementations bind by ID

`checks.yaml` carries what each check verifies and why, with a version that
travels into every report and verdict. A registry entry with no bound
implementation reports an error (exit 2), never a silent pass; a tier that is
off reports a visible skip. The YAML never describes *how* a check runs — that
would turn policy review into code review.

## 2026-10 — Exit contract: error outranks fail

0 pass / 1 blocking fail / 2 could-not-evaluate. A pipeline must be able to
distinguish "evaluated, non-compliant" from "did not evaluate"; folding errors
into failure would let an environmental defect masquerade as a policy verdict.
Warn-severity failures report without failing the run.

## 2026-10 — Three tiers, cheapest first

Static (no image open) → guestfs (read-only filesystem facts) → boot
(behavioral probes in a throwaway QEMU boot). Facts live in the cheapest tier
that can honestly establish them: unit *enablement and ordering* are asked of
systemd in the booted image because drop-ins decide them; file modes and
package sets are read via libguestfs without booting anything; digests and
signatures need no image open at all.

## 2026-10 — guestfs follows the shipped precedent

`os-image/packer/scripts/extract-image-sbom.sh` is the in-repo precedent for
image introspection (libguestfs direct backend, LUKS passphrase delivered to a
`--listen` daemon). The session wrapper copies its conventions rather than
inventing new ones; FDE images open with the operator-supplied passphrase and
fail closed without one.

## 2026-10 — Boot tier probes over SSH via a NoCloud seed

Node images keep cloud-init active by design (SPEC.md), so one probe channel —
a NoCloud seed with a throwaway key, SSH over user-mode networking — serves
real images and test fixtures alike. QEMU runs with `-snapshot`: the artifact
under validation is never written. KVM is used when present; TCG is an
accepted fallback (boot is wait-dominated; measured ≈1.0–1.4× on boot
milestones).

## 2026-10 — Both unit-name generations are valid

The nke→nvke rename (#1548, #1565) lands after os-image v0.2.0, so shipped
releases carry `nke-*` names while new builds carry `nvke-*`. Name-sensitive
checks resolve the generation the image carries (systemd `LoadState`, name
prefixes, the artifact-name pattern) and report which they found. A validator
that broke on either generation would be wrong about a real, signed release.

## 2026-10 — Target compatibility is a BOM over NVKE components

The image deliberately ships no kubeadm/kubelet (the node agent installs the
Kubernetes stack after provisioning), so "built for the right target" is
carried by the baked-in NVKE components: node agent, SPIRE agent, CNI
manifests. COMPAT-002 checks the target BOM against the attested SBOM
(static); COMPAT-003 asks the booted system (boot); INT-004 pins the image to
the SBOM (guestfs). Together: target BOM → attested SBOM → image bytes →
booted system. BOM entries are exact deb name → version pairs, charset-
validated at parse time.

## 2026-10 — Verdict: DSSE attestation, operator-key signing

The run's result is emitted as an in-toto statement (predicate type
`https://nvke.nvidia.com/image-validation/v1`, subject = the image's bare-hex
sha256) in a DSSE envelope signed with an operator-supplied key (Ed25519 or
ECDSA P-256). Keyless/Fulcio signing is deliberately not the default: operator
pipelines in air-gapped sites have no OIDC issuer. `pkg/verify` is the
consumer's half — self-contained, no dependency on the rest of this module —
so the management plane can vendor the smallest possible surface.

Emitted signatures carry **no keyid**: DSSE calls the field advisory, but real
verifiers skip signatures whose keyid does not match their own derivation —
measured with cosign v3.1.3, which reported "accepted signatures do not match
threshold, Found: 0" against an envelope whose only flaw was a keyid. The
cross-verify lane (`hack/cross-verify-cosign.sh`) pins this interoperability:
cosign must accept our verdicts and reject them against different blob bytes.

## 2026-10 — Deferred, visibly

FDE-004 (grub passphrase stripped after the TPM first-boot bind) needs a
swtpm-backed phase-2 boot harness and is deferred in writing, not silently
dropped. The registry gains it when the harness exists; until then it is
absent from checks.yaml rather than present-but-always-skipped.
