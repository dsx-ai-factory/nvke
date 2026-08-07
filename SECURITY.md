## Security

NVIDIA is dedicated to the security and trust of our software products and services, including all source code repositories managed through our organization.

If you need to report a security issue, please use the appropriate contact points outlined below. **Please do not report security vulnerabilities through GitHub public issues.** If a potential security issue is inadvertently reported via a public issue or pull request, NVIDIA maintainers may limit public discussion and redirect the reporter to the appropriate private disclosure channels.

## Reporting a Vulnerability in nke

For vulnerabilities specific to this repository's code, use [GitHub's private vulnerability reporting](https://github.com/NVIDIA/nke/security/advisories/new). This keeps the report confidential while maintainers triage and prepare a fix.

**What to include:**
- Affected version(s) or commit range
- Type of vulnerability (e.g., code execution, privilege escalation, credential exposure)
- Steps to reproduce or a proof-of-concept
- Potential impact

**Response timeline:** Maintainers aim to acknowledge reports within **5 business days** and provide a resolution timeline within **14 business days**. Critical issues will be prioritized and may result in an out-of-band release.

**Scope:** This disclosure path covers vulnerabilities in nke's own code and its direct dependencies. For vulnerabilities in underlying NVIDIA products or infrastructure, use the NVIDIA PSIRT channel below.

**Out of scope for this repository:**
- Vulnerabilities in third-party dependencies not introduced by nke (report upstream)
- Social engineering attacks
- Denial-of-service attacks requiring physical access
- Findings from automated scanners without a working proof-of-concept

**Reporter acknowledgement:** We credit reporters in the GitHub Security Advisory and in release notes (unless you prefer to remain anonymous). Please indicate your preference when filing the report.

## Security guidance for operators

If you are deploying or operating nke:

- **Dependency hygiene:** Pin dependency versions and review Dependabot alerts regularly.
- **Secrets management:** Do not hardcode credentials or API keys. Use environment variables or a secrets manager.
- **Network exposure:** Limit API server and controller access to trusted networks; apply appropriate NetworkPolicies and RBAC.
- **Updates:** Apply security patch releases promptly, and review release notes for supported versions.
- **Supply chain:** Verify release artifact checksums when provided. See release notes for signing details.

## Reporting Potential Security Vulnerability in an NVIDIA Product

To report a potential security vulnerability in any NVIDIA product:
- Web: [Security Vulnerability Submission Form](https://www.nvidia.com/object/submit-security-vulnerability.html)
- E-Mail: psirt@nvidia.com
    - We encourage you to use the following PGP key for secure email communication: [NVIDIA public PGP Key for communication](https://www.nvidia.com/en-us/security/pgp-key)
    - Please include the following information:
        - Product/Driver name and version/branch that contains the vulnerability
        - Type of vulnerability (code execution, denial of service, buffer overflow, etc.)
        - Instructions to reproduce the vulnerability
        - Proof-of-concept or exploit code
        - Potential impact of the vulnerability, including how an attacker could exploit the vulnerability

While NVIDIA currently does not have a bug bounty program, we do offer acknowledgement when an externally reported security issue is addressed under our coordinated vulnerability disclosure policy. Please visit our [Product Security Incident Response Team (PSIRT)](https://www.nvidia.com/en-us/security/psirt-policies/) policies page for more information.

## NVIDIA Product Security

For all security-related concerns, please visit NVIDIA's Product Security portal at https://www.nvidia.com/en-us/security
