# Security Policy: Cloud Workflow Engine

## Reporting a Vulnerability

If you discover a potential security vulnerability, please **do not open a public issue, pull request, or discussion**.

Report it privately through the channels in the repository root
[SECURITY.md](../SECURITY.md):

1. **Preferred:** [NVIDIA Vulnerability Disclosure Program](https://www.nvidia.com/en-us/security/)
2. **Email:** [psirt@nvidia.com](mailto:psirt@nvidia.com)
   - Use the [NVIDIA public PGP key](https://www.nvidia.com/en-us/security/pgp-key) for encrypted communication.

Do not open a GitHub issue, pull request, or GitHub Security Advisory for a
vulnerability report; NVIDIA routes every report through PSIRT.

Include:

- Product or project name and affected version, branch, or commit
- Vulnerability type
- Affected component, file, or interface
- Step-by-step reproduction instructions
- Proof-of-concept code, if available
- Potential impact

Detailed reports help NVIDIA evaluate and address issues faster.

NVIDIA's Product Security Incident Response Team (PSIRT) will acknowledge the report, validate the vulnerability and assess its severity, coordinate development and testing of a fix, and publish a security bulletin when appropriate. Reports are handled under coordinated vulnerability disclosure. NVIDIA does not currently operate a public bug bounty program.

## Security Architecture & Context

Cloud Workflow Engine (CWE) is a Go service that orchestrates NVIDIA Kubernetes Engine cluster, node-pool, and node lifecycle operations. It exposes an OpenAPI-based REST interface, persists desired and observed state in PostgreSQL, and coordinates long-running work through Temporal. It manages Kubernetes and Cluster API resources, communicates with node agents, and integrates with SPIFFE/SPIRE and Teleport identity systems.

The repository also contains the `api` Go module used by other components and the `nico-sim` development service, which emulates NICo using local virtual machines.

CWE operates as a privileged infrastructure service. Its primary security responsibilities are preserving authorization and integrity for cluster lifecycle actions and protecting infrastructure credentials, tenant kubeconfigs, bootstrap data, encryption keys, and machine identity material.

**Repository Exposure Classification:** Public (open source).
Basis: the repository is published at [github.com/dsx-ai-factory/nvke](https://github.com/dsx-ai-factory/nvke) under the Apache-2.0 license.

**Service Exposure Classification:** Sensitive — intended for management-plane deployment, not public exposure.
Basis: the service handles credentials, privileged cluster and node lifecycle operations, and security identity material. Operators should treat any deployment as security-sensitive and restrict it to trusted management networks (see [Deployment Assumptions and Hardening](#deployment-assumptions-and-hardening)).

The principal security boundaries are:

- **REST API boundary:** `internal/server/server.go` exposes cluster, node-pool, node, health, documentation, and UI endpoints. OpenAPI middleware validates request structure, but CWE does not implement end-user authentication or authorization. `X-Managed-By` is a synchronization guard, not an identity credential.
- **Database boundary:** `internal/db` stores cluster, node, network, workflow, security-feature, and reconciliation state in PostgreSQL through Bun. The workflow outbox bridges database state changes into Temporal executions.
- **Temporal boundary:** CWE workers start and execute infrastructure workflows locally and communicate with the CWE and SWE Temporal namespaces. Client mTLS is supported but optional.
- **Kubernetes boundary:** Controllers and activities use the management-cluster service account to create and modify Cluster API resources, Secrets, namespaces, SPIRE entries, and tenant-access resources. Tenant clients are built from CAPI kubeconfig Secrets or kubeconfigs retrieved from control-plane node agents.
- **Node-agent boundary:** `internal/agent/client.go` invokes privileged kubeadm, manifest patching, script execution, kubeconfig retrieval, and node lifecycle RPCs. SPIFFE mTLS is used for SPIFFE-enabled nodes; the compatibility path otherwise uses an unauthenticated plaintext gRPC transport.
- **Identity boundary:** CWE reads its SPIFFE workload identity, creates SPIRE join tokens and static entries, and uses a mounted Teleport identity to manage CWE-owned provisioning tokens and node records.
- **Provisioning-content boundary:** Profiles, CAPI infrastructure manifests, cloud-init files, bootstrap data, embedded runbooks, OIDC configuration, and registry settings influence software and commands executed on provisioned nodes.
- **Operational endpoint boundary:** The primary service listens on configurable HTTP port 8080. Prometheus metrics use a separate listener and the Helm chart enables a namespace-scoped metrics NetworkPolicy by default.
- **Release boundary:** GitHub Actions builds and publishes multi-architecture images, Helm charts, and API module tags using registry, repository, and automation credentials.
- **Development tooling boundary:** `nico-sim` can create, reboot, and delete local VMs, install host routes, expose VM metadata and diagnostic output, and stream console logs. It is intended for isolated development and test use.

### Threat Model

1. **Unauthorized Lifecycle Operations Through the REST API:** The API registered in `internal/server/server.go` has no built-in authentication or authorization. A caller able to reach port 8080 can invoke mutating cluster, node-pool, node, site, and allocation operations that change PostgreSQL state and start privileged Temporal workflows. `managedByMiddleware` only checks a caller-supplied coordination header, is disabled by default, exempts node mutations, and does not establish caller identity.

2. **Execution of Malicious Provisioning Content:** Cluster and node-pool requests can influence CAPI infrastructure manifests, bootstrap files, cloud-init content, image registries, OIDC configuration, and selected profiles. Activities under `internal/temporal/activities` turn these values into Kubernetes Secrets, machine specifications, kubeadm inputs, or agent operations. An unauthorized or compromised caller can therefore cause attacker-controlled configuration or commands to execute with node-level privileges.

3. **Credential, Kubeconfig, or Encryption-Key Disclosure:** CWE handles PostgreSQL passwords, DGXC tenant credentials, Temporal client keys, Teleport identity, SPIFFE material, tenant kubeconfigs, bootstrap Secrets, and Kubernetes API encryption keys. Compromise of the pod, service account, database, mounted Secrets, diagnostic output, or logs can expose credentials that authorize infrastructure or tenant-cluster access. Query and error logging must not reveal secret-bearing values.

4. **Transport Downgrade or Service Impersonation:** The primary REST API serves HTTP directly, PostgreSQL TLS depends on the configured `sslmode`, Temporal mTLS is optional, and agent clients use insecure gRPC when a node is not marked SPIFFE-enabled. Misconfiguration or excessive network reachability can permit interception, credential disclosure, response tampering, or invocation of privileged RPCs by an impersonated service.

5. **Cross-Site, Cross-Cluster, or Cross-Tenant Resource Confusion:** CWE maps database identifiers, infrastructure-provider context, Cluster API objects, kubeconfig Secrets, node addresses, and Temporal workflow identifiers across multiple systems. Stale, malicious, or incorrectly associated state can cause lifecycle operations, credentials, network allocations, or identity records to be applied to the wrong cluster, site, tenant, or node.

6. **Development Emulator Host and VM Compromise:** `nico-sim` listens on all interfaces by default, accepts any non-empty bearer token, exposes selected files and UI paths without authentication, runs an unauthenticated console SSH server, injects a default diagnostic account, and stores cloud-init user data in host-readable files. Its VM, BGP, metadata, diagnostics, and console capabilities can expose bootstrap data or alter host networking if the tool is run outside an isolated development environment.

7. **Release Pipeline or Artifact Compromise:** The GitHub Actions release workflows use privileged repository and registry tokens, publish images and Helm charts to multiple registries, and create API tags. Compromise of a runner, reusable workflow, builder image, dependency source, credential, or registry could distribute modified code or automatically propagate an untrusted version into downstream deployments.

### Critical Security Assumptions

- Network policy, ingress, service-mesh, or API-gateway controls restrict the CWE REST API to authenticated and authorized management clients; CWE itself does not authenticate API callers.
- The `X-Managed-By` header is used only for synchronization ownership and is never treated as proof of identity or authorization.
- Temporal, SWE, Kubernetes, SPIRE, Teleport, PostgreSQL, and agent endpoints authenticate CWE and independently enforce the permissions intended for its service identity.
- The management-cluster service account, mounted workload identities, Kubernetes control plane, and nodes running the CWE pod are trusted and protected against unauthorized access.
- Production deployments use protected network paths and appropriate TLS or mTLS for PostgreSQL, Temporal, agent, Kubernetes, SPIRE, and Teleport traffic; plaintext compatibility paths are confined to trusted networks.
- Kubernetes Secrets, external secret managers, database backups, and persistent volumes are encrypted, access-controlled, audited, and included in credential-rotation procedures.
- Database records, owner references, cluster and node identifiers, infrastructure-provider context, kubeconfig Secrets, and Temporal workflow identifiers accurately describe the resources CWE is authorized to manage.
- Profiles, runbooks, CAPI manifests, cloud-init files, container registries, and other provisioning inputs are maintained by trusted contributors and protected from unauthorized modification.
- Metrics, health responses, logs, workflow history, database query logs, and diagnostics are limited to authorized operators and do not contain reusable credentials, kubeconfigs, bootstrap tokens, encryption keys, or sensitive user data.
- CI runners, shared templates, builder images, private module sources, registries, release credentials, and downstream deployment automation are trusted and protected by the release process.
- `nico-sim` is used only as an isolated development or test tool; its default credentials, unauthenticated console access, metadata service, diagnostic user, host-network privileges, and VM artifacts are never exposed as a production security boundary.
- The container runtime and Kubernetes platform enforce the chart's non-root and read-only-root-filesystem settings, and operators apply resource limits and any additional pod hardening required by the deployment environment.

## Trust Model

CWE treats its reachable REST clients as trusted management-plane actors. It relies on infrastructure outside the application to authenticate those clients and constrain API reachability. A caller authorized to invoke mutating API operations is trusted to request infrastructure provisioning, reconfiguration, reboot, and deletion within the caller's assigned scope.

CWE's own service identity is highly privileged. Depending on enabled features, it can:

- Create, patch, and delete Cluster API and NICo resources
- Create and read bootstrap, kubeconfig, tenant-access, OIDC CA, and encryption-key Secrets
- Modify tenant-cluster Nodes and selected `kube-system` resources
- Execute scripts and kubeadm operations through node agents
- Create SPIRE entries and join tokens
- Manage CWE-owned Teleport tokens and node records
- Start Temporal workflows that carry out these actions

Downstream systems remain responsible for independently validating CWE's credentials and enforcing organization, tenant, namespace, resource, and identity scope.

## Deployment Assumptions and Hardening

Operators should:

- Keep the REST service internal and place it behind authenticated, authorized management-plane access controls.
- Treat `X-Managed-By` as coordination metadata only; do not use it as an authentication mechanism.
- Restrict ingress to port 8080 and keep the metrics NetworkPolicy enabled with only approved scraper namespaces.
- Enable Temporal mTLS and PostgreSQL certificate verification where supported by the environment.
- Enable SPIFFE for production agent communication and prevent network access to legacy plaintext agent ports.
- Use per-environment service identities and narrowly scoped Kubernetes RBAC, database roles, and Teleport permissions.
- Source secrets from an approved secret manager, rotate them after suspected disclosure, and avoid placing credentials directly in Helm values or environment files.
- Protect tenant kubeconfigs, bootstrap Secrets, encryption keys, and cloud-init content with the same controls used for other cluster-administrator credentials.
- Restrict access to logs, metrics, readiness responses, Temporal history, and PostgreSQL diagnostics.
- Pin release artifacts by digest where possible and review changes to shared CI templates, builder images, dependencies, and publish jobs.
- Run `nico-sim` only on isolated development hosts and bind its HTTP, metadata, BGP, and console listeners to explicitly controlled interfaces.

## Security Scope and Common Scanner Context

The following patterns are expected and are not vulnerabilities by themselves:

- Placeholder PostgreSQL passwords, diagnostic credentials, endpoints, certificates, and kubeconfigs in local-development configuration, documentation, or test fixtures are not production credentials.
- Support for custom CA bundles and optional mTLS is intentional. A report should demonstrate an unauthorized downgrade, validation bypass, or concrete exposure rather than only note that configuration is optional.
- Kubernetes Secret access, tenant kubeconfig handling, agent script execution, and infrastructure lifecycle calls are required for CWE's documented role. Reports should demonstrate an authorization bypass, unintended disclosure, cross-scope operation, or avoidable privilege expansion.
- `nico-sim` deliberately includes weaker local-development controls. Reports should demonstrate impact outside its documented isolated development trust boundary.

The following remain valid security concerns and should not be dismissed as expected behavior:

- CWE's REST API becoming reachable without the intended upstream authentication and authorization
- Unauthorized production lifecycle actions or execution of provisioning content
- Disclosure of production credentials, tenant kubeconfigs, bootstrap data, or encryption keys
- Cross-cluster, cross-site, cross-tenant, or cross-namespace authorization bypass
- Production use of insecure agent or service transport contrary to deployment policy
- Release artifact tampering or CI/CD credential leakage
- Exposure of `nico-sim` privileged development interfaces beyond an isolated test environment
