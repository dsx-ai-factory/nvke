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
root is pinned by the operator out-of-band (`--trusted-root`) — deliberately
never read from the evidence directory, whose contents are exactly what the
root is needed to authenticate; verification uses observer timestamps, not
Rekor lookups. This is also why the module avoids any client SDK that assumes
registry or transparency-log reachability.

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

## 2026-10 — Completeness is the consumer's policy, and it is enforceable

An authentic signature over a skip-heavy run must never satisfy a gate that
expected full validation (found by independent review: digest-only mode with
tiers off yields two passes, fifteen skips, exit 0, a signed pass verdict).
Three mechanisms close it:

- Skips carry a machine-readable reason: `not-applicable` (the target is
  outside the check's appliesTo) vs `tier-off` / `input-missing` (omitted
  validation). An implementation skip without a reason is an engine error.
- Verdicts bind their policy inputs: `evidenceMode` (bundle vs digest-only),
  `checkSetSha256`, `bomSha256` — the version string names the policy, the
  digests pin its content.
- `verify.Policy.RequiredChecks` (and `nvke-verdict --required-checks`) is the
  consumer's qualification profile: each listed check must have passed or be
  not-applicable. The list lives with the consumer, deliberately independent
  of the verdict's contents — a signer cannot shrink the profile by omitting
  results.

## 2026-10 — Policy is enforced by pinning, not by recording

Second independent-review round. Required-check completeness is not enough on
its own: applicability comes from the caller-selected registry, so a registry
whose `appliesTo` selectors never match the target turns every required check
into an acceptable not-applicable skip (demonstrated with a one-letter arch
typo across all selectors — 17 skips, signed pass, accepted). The verdict
*recording* `checkSetSha256` describes; only the consumer *comparing* it
enforces. Hence `Policy.CheckSetSHA256` (approved registry digests) and
`Policy.ExpectedTarget` — and the documented trust boundary: without those
pins, applicability belongs to the producer. Same round: duplicate result IDs
are rejected at verification (last-write-wins would let a signed
fail-then-pass pair read as pass); policy inputs are read once and the same
byte snapshot is parsed and hashed (a re-read at signing time could hash
different content than the run used); and exactly one report document ever
reaches stdout — a signing failure rides the report as `runError` instead of
appending a second JSON object.

## 2026-10 — One snapshot per input, end to end

Third independent-review round extended the snapshot rule to the evidence
itself: the bundle authenticated one read of checksums.txt while the digest
map came from a second open, so a race-swapped file could pair an authentic
INT-002 pass with digests the attestation never covered (reproduced against
the real release fixture). Checksums now parse from the same bytes the
attestation subject hashes. The image — too large to snapshot in memory — is
re-hashed after any inspection tier reopened it by path, and a mismatch
aborts the run. Same round: unsupported formats are rejected before any side
effect (an invalid --format briefly left a signed verdict behind an exit 2),
the verdict path must not alias the report stream, duplicate result IDs are
rejected structurally rather than only under a profile, and malformed policy
pins are errors instead of inert entries. Immutable staged inputs remain the
recommended pipeline posture; these checks catch violations rather than
excuse them.

## 2026-10 — Linux is the runtime; devices resolve at open time

The validator deploys on Linux (operator build pipelines); macOS is a
development convenience. Fourth review round showed why output-destination
guards cannot rely on path stat alone: on macOS /dev/stdout is a character
device whose path stat never matches the redirect target. The verdict writer
therefore opens its destination first and compares descriptors with the
report sink, writing through the checked handle. Evidence from acceptance
runs is exported (report, verdict, key, BOM, input digests, tool versions) so
results are archivable facts rather than narrated ones.

## 2026-10 — Deferred, visibly

FDE-004 (grub passphrase stripped after the TPM first-boot bind) needs a
swtpm-backed phase-2 boot harness and is deferred in writing, not silently
dropped. The registry gains it when the harness exists; until then it is
absent from checks.yaml rather than present-but-always-skipped.
