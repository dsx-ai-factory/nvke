# Security Policy

NVIDIA is dedicated to the security and trust of our software products and
services, including all source code repositories managed through our
organization.

## Reporting a vulnerability

**Report security vulnerabilities to NVIDIA PSIRT.** Do not open a GitHub
issue, a pull request, or a GitHub Security Advisory. NVIDIA product security
policy routes every external vulnerability report through PSIRT so that
disclosure is coordinated across all affected NVIDIA products, not just this
repository.

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

PSIRT acknowledges receipt of every report, coordinates with the reporter
through the investigation, and provides progress updates as remediation
proceeds. See [PSIRT Policies](https://www.nvidia.com/en-us/security/psirt-policies/)
for NVIDIA's full coordinated-disclosure policy.

Alongside that process, the maintainers of this repository aim to:

| Stage | Target |
|---|---|
| Acknowledge a report forwarded by PSIRT | 5 business days |
| Confirm or reject, with a severity assessment | 10 business days |
| Give the reporter a remediation timeline | 14 business days |

If you have not heard anything within a few business days of filing, resend to
psirt@nvidia.com rather than opening a public issue.

## Remediation targets

Severity is assessed with CVSS v3.1. These are the targets the maintainers work
to once a report is confirmed; PSIRT owns the disclosure date.

| Severity | CVSS v3.1 | Target fix available |
|---|---|---|
| Critical | 9.0 – 10.0 | 14 calendar days, out-of-band release |
| High | 7.0 – 8.9 | 30 calendar days |
| Medium | 4.0 – 6.9 | 90 calendar days, next scheduled release |
| Low | 0.1 – 3.9 | Next scheduled release |

Dependency vulnerabilities are picked up by Dependabot and follow the same
targets, measured from the date a fixed upstream version becomes available.

## Embargo and coordinated disclosure

Confirmed vulnerabilities are handled under embargo. PSIRT manages the
timeline: the issue stays confidential while a fix is prepared, and an advisory
is published when the embargo lifts. NVIDIA is a CVE Numbering Authority and
assigns CVE identifiers for resolved issues that require action from users.

Maintainers support this by developing fixes privately and holding public
discussion — issues, pull requests, commit messages and release notes — until
PSIRT lifts the embargo. Please keep anything you report confidential until the
advisory is published.

## Supported versions

nke has not yet published a release. Until it does, `main` is the only branch
that receives security fixes.

| Version | Supported |
|---|---|
| `main` | ✅ Receives security fixes |
| Pre-release commits and forks | ❌ Not supported |

Once releases begin, this table will name the release lines that receive
backports, and the policy will be to support the latest minor release only
until the project reaches 1.0.

## Reporter credit

We credit reporters by name in the published advisory and in the release notes
of the release that carries the fix, unless you ask to stay anonymous. Tell
PSIRT your preference, and the name or handle you want used, when you file.

NVIDIA does not run a bug bounty program. We do acknowledge externally reported
issues resolved under our coordinated vulnerability disclosure policy.

## Artifact integrity

**nke has published no releases, so there is no signed artifact to verify
today. Treat any binary, image, or package claiming to come from this project
as unverified.**

When releases begin, every release artifact will be published with a SHA-256
checksum file, and container images and binaries will be signed with
[cosign](https://docs.sigstore.dev/) keyless signing. Verification instructions
will be published in [RELEASE.md](RELEASE.md) alongside the first signed
release. Third-party dependency licenses for the `agent` module are recorded in
[agent/THIRD_PARTY_NOTICES.md](agent/THIRD_PARTY_NOTICES.md), regenerated and
verified in CI on every push.

## Out of scope

These generally do not qualify as vulnerabilities in nke:

- Vulnerabilities in upstream components — Kubernetes, kubeadm, containerd,
  Temporal, PostgreSQL, SPIRE, Cluster API. Report those to the owning project.
  If nke's use of one of them makes an upstream issue exploitable in a way it
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

## NVIDIA Product Security

For all other security concerns, see NVIDIA's
[Product Security portal](https://www.nvidia.com/en-us/security).
