# Security Policy

NVIDIA is dedicated to the security and trust of our software products and
services, including all source code repositories managed through our
organization.

## Reporting a vulnerability

**Report security vulnerabilities to NVIDIA PSIRT.** Do not open a GitHub
issue, a pull request, or a GitHub Security Advisory to report a vulnerability.
NVIDIA product security policy routes every external report through PSIRT so
that disclosure is coordinated across all affected NVIDIA products, not just
this repository.

- **Web:** [Security Vulnerability Submission Form](https://www.nvidia.com/object/submit-security-vulnerability.html)
- **Email:** psirt@nvidia.com — encrypt with the [NVIDIA public PGP key](https://www.nvidia.com/en-us/security/pgp-key)

Include as much of the following as you have:

- The affected version, tag, or commit SHA
- Which component is affected — `controller/` (Cloud Workflow Engine) or
  `agent/` (node agent)
- The type of vulnerability: remote code execution, privilege escalation,
  credential exposure, authentication bypass, denial of service, and so on
- Steps to reproduce, and a proof of concept if you have one
- The impact, and how an attacker would reach the vulnerable code

If a security issue is reported publicly by mistake, maintainers will limit
discussion on the public thread and redirect the reporter to PSIRT.

## Response and acknowledgement

PSIRT coordinates with reporters through the investigation and provides
progress updates as appropriate. See
[NVIDIA PSIRT Policies](https://www.nvidia.com/en-us/security/psirt-policies/)
for the public coordinated-disclosure and remediation policy. The response
and resolution timeline depends on the case; this repository does not set a
separate public deadline.

If you need to follow up on a report, contact psirt@nvidia.com rather than
opening a public issue.

## Embargo and coordinated disclosure

Confirmed vulnerabilities are handled under embargo. PSIRT manages the
timeline: the issue stays confidential while a fix is prepared, and any NVIDIA
security bulletin or CVE information is published only when PSIRT authorizes
disclosure. NVIDIA is a CVE Numbering Authority and assigns CVE identifiers
for resolved issues that require action from users.

Maintainers support this by developing fixes privately and holding public
discussion — issues, pull requests, commit messages and release notes — until
PSIRT lifts the embargo. Please keep anything you report confidential until
PSIRT authorizes disclosure.

For fix development, maintainers may use an **unpublished draft** GitHub
Security Advisory only to create a temporary private fork. It is not a
reporting channel and must not be published.

## Supported versions

Each component releases on its own tag line (`agent/vX.Y.Z`, `cli/vX.Y.Z`,
`controller/vX.Y.Z`, `os-image/vX.Y.Z`, `nktp/vX.Y.Z`) and a product release
`vX.Y.Z` ties them together; the first stable releases shipped on 2026-09-30.
The `controller/api` Go module publishes `v0.0.x` so consumers can pin. There
are no long-lived release branches: a security fix lands on `main` and ships in
the next release of the affected component, or as a hotfix release cut from the
patched tag as [RELEASE.md](RELEASE.md) describes.

| Version | Supported |
|---|---|
| `main` | ✅ Receives security fixes |
| The latest release of each component line | ✅ Fixed in its next release, or a hotfix release |
| Older releases, pre-releases and forks | ❌ Not supported |

Until the project reaches 1.0 the policy is to support the latest minor release
of each component only.

## Reporter credit

NVIDIA may credit reporters in a published security bulletin or related release
notes, in coordination with PSIRT. Tell PSIRT if you prefer anonymity, or the
name or handle you would like used, when you file.

NVIDIA does not run a bug bounty program. We do acknowledge externally reported
issues resolved under our coordinated vulnerability disclosure policy.

## Artifact integrity

**Every component release attaches a SHA-256 `checksums.txt` with a
build-provenance attestation over it, and the images published to GHCR are attested by digest.
Verify the attestation, then the checksums, and treat any artifact that fails
either as unverified.**

Release artifacts and the GHCR container images carry
[GitHub build-provenance attestations](https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds):
keyless Sigstore, signed against the GitHub Actions OIDC token, recording the
workflow, ref and commit that produced the artifact, and logged in a public
transparency log. There is no signing key held by this project.

[RELEASE.md](RELEASE.md) carries the verify commands and what each attestation
covers. Always pass `--repo dsx-ai-factory/nvke` — that is what binds an
attestation to this repository rather than to any GitHub workflow anywhere.

A checksum alone proves a download arrived intact, not who produced it: whoever
can replace an artifact can replace the checksum file beside it. That is what
the attestation adds.

The NVCR and DSX copies of the published images are not yet attested, because
those registries do not serve the OCI referrers API. No SLSA level is claimed.

Third-party dependency licenses are recorded per component in
[agent/THIRD_PARTY_NOTICES.md](agent/THIRD_PARTY_NOTICES.md),
[cli/THIRD_PARTY_NOTICES.md](cli/THIRD_PARTY_NOTICES.md),
[controller/THIRD_PARTY_NOTICES.md](controller/THIRD_PARTY_NOTICES.md), which
also covers the provisioned provider, and
[os-image/THIRD_PARTY_NOTICES.md](os-image/THIRD_PARTY_NOTICES.md), indexed by
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md); all four are regenerated and
verified in CI on every push.

## Out of scope

These generally do not qualify as vulnerabilities in NVKE:

- Vulnerabilities in upstream components — Kubernetes, kubeadm, containerd,
  Temporal, PostgreSQL, SPIRE, Cluster API. Report those to the owning project.
  If NVKE's use of one of them makes an upstream issue exploitable in a way it
  otherwise would not be, that **is** in scope.
- Findings that require existing root on a managed node, or cluster-admin on
  the management cluster, to exploit.
- Missing hardening that is the operator's responsibility: RBAC, network
  policy, secrets management, disk encryption settings, ingress exposure.
- Denial of service that needs physical access to the hardware.
- Scanner output with no demonstrated, practical exploit path.
- Reports against a deployment operated by someone else — contact that operator.

When in doubt, report it. PSIRT would rather triage something out of scope than
miss a real issue.

## Guidance for operators

If you deploy or operate nke:

- **Secrets.** Never commit credentials, API keys, tokens, or kubeconfigs.
  Supply them through the environment or a secrets manager. The database
  connection string is read from the environment for this reason.
- **Node identity.** The agent authenticates with SPIFFE/SPIRE mTLS by default.
  `--disable-mtls` exists for local development only — never enable it on a
  network anyone else can reach.
- **Network exposure.** Keep the controller's API behind authentication and
  restrict it to trusted networks. The agent's gRPC port is meant for the
  control plane, not for general access.
- **Updates.** Apply security releases promptly and read the release notes for
  the supported-version policy in force at the time.
- **Dependencies.** Review Dependabot alerts and keep pinned versions current.
- **Pod Security Admission.** NVKE labels its own system namespaces
  `privileged` — kube-proxy, the CNI, and node-local-dns all need host
  access, which is standard practice for these namespaces. Applying Pod
  Security Admission to your own namespaces is your responsibility.

## NVIDIA Product Security

For all other security concerns, see NVIDIA's
[Product Security portal](https://www.nvidia.com/en-us/security).
