# Agent Security Context

Vulnerability reporting for this monorepo lives in the repository-root [SECURITY.md](../../SECURITY.md).

## Security Architecture and Context

### Purpose and software class

NVKE agent (`nvke-agent`) is a privileged Go gRPC service installed as a
systemd unit on NVIDIA Kubernetes Engine nodes. The NVKE controller
calls it to perform node-local Kubernetes lifecycle and administration tasks
that would otherwise require root shell access.

The service:

- initializes, joins, resets, and inspects Kubernetes nodes through `kubeadm`;
- installs NVKE bundles, Debian packages, apt repositories, and CNI components;
- configures Kubernetes API/etcd encryption and rotates encryption keys;
- changes control-plane static pod manifests and kube-vip state;
- uploads files, executes scripts, and collects support bundles.

The implemented API is defined in `pkg/agent/proto/node_agent.proto`. The
server entry point and transport policy are in `cmd/agent/main.go`; privileged
RPC implementations are under `internal/agent/`.

### Protected assets

- Root-level control of each NVKE node and its system configuration.
- Kubernetes administrator kubeconfig and kubeadm join credentials.
- Kubernetes API encryption keys and LUKS master keys passed in RPC requests.
- SPIFFE X.509 SVIDs and the SPIRE workload API trust root.
- `/var/lib/nvke/state.json`, including lifecycle and operation state.
- Kubernetes static pod manifests and cluster RBAC resources.
- NVKE packages, bundles, signing keys, indexes, and OCI credentials.
- Audit logs, script-execution logs, and support bundles, which may contain
  sensitive operational information.

### Trust boundaries and data flows

1. **Controller to agent:** The NVKE controller connects to TCP port `15005`. Production
   connections use SPIFFE mTLS. `cmd/agent/main.go` requires the client's SVID
   to share the agent's trust domain and have a path beginning with
   `/nke-service/`.
2. **SPIRE to agent:** The agent obtains its X.509 identity and trust
   bundle from the SPIRE workload API socket configured by
   `--spiffe-socket-path`.
3. **agent to the host:** The systemd service runs as root and invokes
   tools such as `kubeadm`, `apt-get`, `helm`, `kubectl`, `cryptsetup`,
   `clevis`, and `crictl`. It writes security-sensitive host and Kubernetes
   configuration.
4. **agent to Kubernetes:** On control-plane nodes, the agent uses the
   administrator kubeconfig for cluster mutations. Local API-server health
   checks target `https://127.0.0.1:6443`; TLS verification is deliberately
   skipped for this loopback-only local endpoint.
5. **agent to artifact services:** Bundle and collection operations fetch
   content from configured HTTPS/S3 or OCI sources. Apt repositories use
   caller-supplied signing keys and `signed-by` keyrings.
6. **agent to operators:** Unary RPC metadata is written to
   `/var/log/nvke-agent-audit.log` and mirrored to the service journal. Script
   activity is written to `/var/log/nvke-execute-scripts.log`.
   `GetSupportBundle` returns selected state and logs to an authenticated caller.

### Authorization model

SPIFFE mTLS authenticates and coarsely authorizes the calling NVKE service.
Possession of an authorized `/nke-service/` identity grants access to the full
gRPC service; the agent does not implement per-caller or per-RPC roles.
Individual RPCs add validation where applicable, such as control-plane role
checks and file path and size constraints.

This broad authority is intentional because the NVKE controller is the trusted
cluster orchestrator. Compromise of an authorized controller identity must therefore be
treated as potential root compromise of every node reachable by that identity.

## Threat Model

### 1. Unauthorized use of privileged gRPC operations

An attacker who reaches port `15005` and obtains an authorized
`/nke-service/` SPIFFE identity could invoke `ExecuteScript`,
`StreamExecuteScript`, `UploadFileBundle`, kubeadm operations, encryption
operations, or package installation as root.

Controls include SPIFFE mTLS in `cmd/agent/main.go`, same-trust-domain and path
prefix checks, network segmentation, unary RPC audit logging, and
operation-specific validation. The SPIRE control plane and authorized controller
identities are therefore critical security dependencies.

### 2. Plaintext gRPC enabled outside development

Starting `cmd/agent/main.go` with `--disable-mtls` removes transport
authentication and encryption while retaining the full privileged API and
gRPC reflection. Any network principal able to reach the port could then
control the node.

The flag and plaintext `grpcurl` examples exist for isolated development only.
Production deployment configuration must never set `--disable-mtls`, and the
node network must restrict access to port `15005`.

### 3. Malicious scripts, archives, or staged files

`internal/agent/agent.go` and related upload and execution paths intentionally
support remote file placement and arbitrary script execution. Crafted archive
paths, unsafe destinations, oversized files, shell content, or misuse by an
authorized caller could modify the host or expose secrets.

Staged kubeadm files require canonical absolute paths, valid ownership and
permission values, and a per-file size limit. Upload and execution activity is
audited. These checks reduce accidental misuse but do not sandbox content from
an authorized orchestrator.

### 4. Artifact or repository supply-chain compromise

`InstallBundle`, `InstallCollection`, `ConfigureAptRepo`, and
`InstallPackages` can install root-executed content obtained from configured
S3, HTTPS, apt, or OCI locations. A compromised publisher, registry,
repository signing key, index, credential, or configuration could introduce
malicious node software.

Production deployments must use NVKE-controlled artifact locations, protect OCI
credentials such as `NGC_API_KEY`, validate apt metadata with dedicated
`signed-by` keyrings, and preserve publisher-side access controls and release
provenance. Development S3 locations and `file://` sources are not production
trust anchors.

### 5. Disclosure of cluster and disk-encryption secrets

Kubeadm join tokens, administrator kubeconfig, Kubernetes API encryption keys,
and LUKS passphrases cross the gRPC boundary. Debug logging, command-line
construction, error messages, state persistence, audit output, or support
bundles could expose those values.

Encryption and LUKS keys are passed in memory or through process standard input
and must not be logged. Callers must protect responses from `GetKubeconfig`,
`GetKubeadmJoinString`, and `GetSupportBundle`, redact logs, and rotate exposed
credentials immediately.

### 6. Unsafe or inconsistent control-plane mutation

Interrupted or incorrectly ordered operations affecting
`encryption-config.yaml`, kube-apiserver static pod manifests, kube-vip, or etcd
object rewrites could reduce availability or leave data readable with an old
key. Supplying different encryption keys to control-plane nodes could make
cluster data unavailable.

The newer implementations use operation IDs, persisted status, serialized
mutation, write-ahead state, readiness checks, snapshots, rollback, and
explicit terminal outcomes. The NVKE controller remains responsible for cross-node sequencing
and for applying identical encryption configuration to every control-plane
node.

### 7. Excessive data returned in support artifacts or status

`GetSupportBundle` packages service logs, audit logs, script logs, package
inventory, state, and caller-selected files. Status RPCs and support-bundle responses may
also reveal topology or lifecycle details. An authorized but compromised
caller could use these interfaces for reconnaissance or data collection.

Only trusted orchestration and support identities may call these RPCs.
Operators must request only necessary additional files, handle returned
archives as sensitive, and apply retention and access controls outside this
repository.

## Critical Security Assumptions

- Production starts the agent with SPIFFE mTLS enabled and does not use
  `--disable-mtls`.
- SPIRE securely attests workloads, protects its signing keys and workload API
  socket, rotates SVIDs, and issues `/nke-service/` identities only to trusted
  NVKE services.
- Network policy, host firewalls, and site routing limit TCP port `15005` to
  authorized orchestration paths.
- The NVKE controller authenticates its operators, authorizes requested workflows, validates
  tenant and node ownership, protects sensitive RPC responses, and safely
  sequences multi-node operations.
- The node OS, systemd unit, root filesystem, SPIRE agent, and executables
  invoked by agent are trusted and protected from unprivileged mutation.
- Production artifact repositories, OCI registries, apt signing keys, and
  bundle indexes are controlled by NVKE release infrastructure and are not
  writable by untrusted principals.
- Kubernetes administrator kubeconfig, encryption configuration, and static pod
  manifests have root-only host permissions.
- The loopback endpoint `127.0.0.1:6443` cannot be redirected to an untrusted
  service; this is the basis for skipping TLS verification in the local
  API-server client.
- Audit and script logs are collected, access-controlled, rotated, and monitored
  by deployment infrastructure. The application does not provide remote log
  integrity or retention guarantees.
- The agent owns one node for one NVKE cluster. It is not designed to enforce
  isolation among mutually untrusted tenants on the same host.

## Deployment Assumptions and Hardening

- Run the packaged systemd service as root only because its host-management
  responsibilities require it; do not grant additional Linux capabilities or
  mount access to unrelated workloads.
- Fail deployment if the SPIRE workload API is unavailable rather than falling
  back to plaintext.
- Restrict and monitor port `15005`; do not expose it through a public load
  balancer.
- Protect `/etc/default/nvke-agent`, the SPIRE socket, `/var/lib/nvke`,
  `/etc/kubernetes`, apt keyrings, and `/var/log/nvke-*` with root-only write
  access.
- Use production-controlled HTTPS or OCI bundle sources and dedicated apt
  signing keys. Do not use arbitrary caller-controlled repositories.
- Treat every authorized `/nke-service/` SVID as a privileged administrative
  identity and keep its scope and lifetime as small as the platform permits.
- Monitor calls to script execution, file upload, kubeconfig retrieval,
  encryption, RBAC, repository configuration, and package installation RPCs.
- Securely delete temporary sensitive artifacts and avoid adding secret-bearing
  files to support bundles.

## Out of Scope

- Vulnerabilities in Kubernetes, kubeadm, SPIRE, container runtimes, the host
  operating system, or third-party packages that are not caused by agent
  integration remain owned by their respective upstream projects.
- Authentication and authorization of human operators are implemented by the NVKE
  controller and the surrounding NVKE platform, not by agent.
- Cross-node orchestration, tenant ownership decisions, backup, disaster
  recovery, network segmentation, log retention, and artifact publication
  infrastructure are external controls.
- Plaintext operation on an isolated developer machine is supported for local
  testing but is not a supported production security posture.

## Accepted Risks

No Argus findings are currently recorded as accepted risks. A future accepted
risk must identify the concrete exposure, scope, owner, expiration or review
condition, and compensating control here, with a matching entry in
`.security-triage.yaml`.

## Common False-Positive Patterns

These patterns require validation against the reported path and execution
context; they are not blanket suppressions:

- `grpcurl -plaintext` and `--disable-mtls` in `README.md`, scripts, or tests
  describe local development. Plaintext operation in production is a valid
  vulnerability.
- Example kubeadm tokens, encryption keys, passwords, certificates, or
  kubeconfigs in documentation and test fixtures are non-production values only
  when their provenance confirms they cannot authenticate to a live system.
- `Insecure: true` or skipped TLS verification in `internal/k8s/registry.go` is
  restricted to the loopback API-server endpoint `127.0.0.1:6443`. The same
  behavior for a non-loopback or caller-controlled endpoint is a valid issue.
- `math/rand` used for retry jitter or non-security identifiers is not a
  cryptographic use. Randomness used for credentials, keys, tokens, or
  authorization decisions remains security-sensitive.
- Root command execution and file writes under `internal/agent/` are core
  product capabilities, but missing authentication, input-boundary validation,
  secret handling, or audit controls around them remain valid findings.

## Report Acceptance Gate

A report should identify a concrete path from attacker-controlled input or an
untrusted principal to a security impact in a supported deployment. Reports
that rely on development-only plaintext mode, example-only credentials, or an
already-root attacker must explain why that condition can occur in production.

This gate does not exclude bypasses of SPIFFE authorization, cross-trust-domain
access, archive or path traversal, command injection, secret disclosure,
artifact-verification failures, unsafe control-plane mutation, or privilege
escalation from a less-trusted NVKE identity.
