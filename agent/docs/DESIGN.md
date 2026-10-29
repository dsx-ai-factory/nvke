# NVKE Agent — Design Document

> Status: living document / first pass. This describes the agent **as it is
> actually implemented today**, not an aspirational design. The project has grown
> organically over several years; this doc is an attempt to retro-fit a coherent
> design narrative onto the current code so it can be reasoned about, reviewed,
> and evolved deliberately.

---

## 1. Overview

The **NVKE agent** (`nvke-agent`) is a Go gRPC server that runs as a systemd
service on every node of an NVKE (NVIDIA Kubernetes Engine) cluster. It is the
**on-node execution surface** for cluster orchestration: the NVKE controller
(the consolidated orchestrator under `controller/` in this monorepo) drives
cluster lifecycle by issuing gRPC calls to the agent on each node rather than
SSH-ing in and running commands ad hoc.

In one sentence: **the agent turns privileged, node-local imperative
operations (kubeadm, apt, disk encryption, static-pod manipulation, script
execution) into an authenticated, auditable, idempotent-where-it-matters gRPC
API.**

### Primary responsibilities

- **Kubernetes lifecycle via kubeadm** — init, join, reset, preflight, join-string
  and kubeconfig retrieval.
- **CNI installation** — deploy packaged OVN / Calico charts or manifests into the
  cluster (current path), plus a legacy in-init CNI path retained for older
  orchestrators.
- **Software/package management** — install NVKE "bundles" (tgz), apt packages,
  apt repo configuration, and "collections" of either.
- **Kubernetes API/etcd secrets encryption at rest** — initialize, promote
  providers, key rotation, and object rewrite, all coordinated across control-plane
  nodes.
- **Control-plane graceful shutdown / kube-vip drain & restore** — cordon this
  node's control-plane traffic without disrupting cluster availability.
- **Node introspection & support** — status, node info, support-bundle collection,
  arbitrary (audited) script execution, file upload.

### What it is *not*

- It is **not** a controller/reconciler. It does not watch the API server and
  converge desired state on a loop. It executes discrete, mostly imperative RPCs
  and reports status. (A few operations are "convergent/idempotent" in the sense
  that re-issuing them is safe — see §7 — but there is no background control loop.)
- It is **not** the orchestrator. Sequencing across nodes (e.g. "init one CP, then
  join the rest, then install CNI on one node") lives in the NVKE controller, not
  the agent.
- It is **not** cloud/infra specific. It targets Ubuntu-based NVKE OS images and is
  otherwise infrastructure-agnostic.

---

## 2. Background & evolution

The agent began as a thin "run kubeadm over gRPC" service and accreted
capabilities as NVKE's needs grew. That history is visible in the code and the API:

- Deprecated stand-alone RPCs (`InstallKubeadm`, `UpgradeAgent`, `ApplyNodeProfile`,
  `InstallCNIPlugin`, `RewriteEncryptedObjects`, `UpdateKubernetesAPIEncryptionKey`,
  `GetNodeInfo`, `BootstrapNode`) have been **removed** from the proto service as
  part of the GitHub monorepo migration.
- Bundle installation has migrated from bespoke tgz "bundles" and "node profiles"
  toward **apt packages + an S3/OCI-backed apt repo** (see the sibling
  `apt-publisher` project).
- CNI install moved from **in-`kubeadm init`** installation to a **separate
  `InstallCNI` RPC** driven off Debian-packaged charts. A legacy **internal**
  in-init path remains when `no_cni_install` is false for older orchestration.
- Transport moved from plaintext gRPC to **SPIFFE mTLS** by default.
- Newer subsystems (graceful shutdown, API-server manifest patching, etcd
  encryption rewrite) adopt more disciplined patterns — async operation framework,
  two-lane outcome contracts, WAL-style resumability — that older RPCs predated.

Design implication: **prefer the newer patterns (§7)** for all new work.

---

## 3. Goals & non-goals

### Goals

- Provide a **single, authenticated, audited entry point** for privileged node
  operations, so orchestration does not need interactive shell access.
- Be **safe to call repeatedly** for the operations where retries are expected
  (kubeadm status polling, encryption reload waits, async operations keyed by ID).
- **Survive agent restarts** without corrupting node state (atomic state file,
  resumable/loud-on-restart operation semantics).
- **Preserve cluster availability** during disruptive node operations (graceful
  shutdown, apiserver manifest patching self-restore on failure).
- Remain **discoverable** without shipping the `.proto` to every client (gRPC
  reflection), so orchestrators can feature-detect RPCs.

### Non-goals

- Cross-node coordination / cluster-wide state (owned by the NVKE controller).
- General-purpose remote shell (script execution exists but is audited and scoped).
- Multi-tenancy on a single node — one agent owns the node.

---

## 4. System context

                        ┌─────────────────────────────┐
                        │      NVKE controller         │
                        │        (orchestrator)        │
                        └───────────────┬──────────────┘
                                        │ gRPC (SPIFFE mTLS), per-node
             ┌──────────────────────────┼──────────────────────────┐
             ▼                          ▼                          ▼
   ┌───────────────────┐      ┌───────────────────┐      ┌───────────────────┐
   │   node (CP)       │      │   node (CP)       │      │   node (worker)   │
   │  ┌─────────────┐  │      │  ┌─────────────┐  │      │  ┌─────────────┐  │
   │  │  nvke-agent  │  │      │  │  nvke-agent  │  │      │  │  nvke-agent  │  │
   │  └──────┬──────┘  │      │  └──────┬──────┘  │      │  └──────┬──────┘  │
   │  kubeadm/apt/     │      │  kubeadm/apt/     │      │  kubeadm/apt/     │
   │  crictl/cryptsetup│      │  crictl/cryptsetup│      │  ...              │
   │  local apiserver  │      │  local apiserver  │      │                   │
   └───────────────────┘      └───────────────────┘      └───────────────────┘
             ▲
             │ X.509 SVIDs
   ┌───────────────────┐
   │  SPIRE agent      │  (workload API socket)
   └───────────────────┘

External sources:
  - Bundle/apt artifacts: S3 (https://…s3…/nke_bundles, nke-apt-*) or OCI (nvcr.io)
  - CNI charts/manifests + images: NVKE Debian packages (nvke-cni-ovn-*, nvke-cni-calico-*)
```

- **Caller:** The NVKE controller is the primary caller. `grpcurl` is used for
  dev/debug and is first-class thanks to reflection.
- **Identity:** SPIRE issues X.509 SVIDs; the agent authorizes clients by trust
  domain and a `/nke-service/` SPIFFE path prefix.
- **Artifact sources:** bundles/packages come from S3 or an OCI registry; CNI
  content comes from NVKE-published Debian packages.
- **Local apiserver:** on control-plane nodes the agent talks to the *local*
  apiserver (`https://127.0.0.1:6443`, TLS-skip) for reload/readiness waits, and to
  the cluster apiserver via the admin kubeconfig for cluster mutations.

---

## 5. Process model & lifecycle

Entry point: `cmd/agent/main.go`.

1. **Flag/config parse** → `AgentConfig` (listen port, bundle source URL, index
   URL, CNI image registry, mTLS toggle, SPIFFE socket, audit/script log paths,
   mlx5 rescan toggle).
2. **Logger setup** — three JSON `slog` loggers: default (stdout), **audit**
   (`/var/log/nvke-agent-audit.log` and stdout for journald), **script execution**
   (`/var/log/nvke-execute-scripts.log`).
3. **`RunNodeStartup`** → `NewNodeAgentServer`:
   - Ensure `/var/lib/nvke` and `state.json` exist (never truncate existing state).
   - Optionally parse the bundle source URI (HTTP/S3/`file://`/`oci://`). When
     unset or invalid, log a warning and continue — apt is preferred.
   - Resolve the bundle index file (Ubuntu 22 vs 24) when a bundle source is set,
     unless overridden by `--index-file-url`.
   - Load persisted `NodeStatus` from `state.json`; normalize legacy values
     (`UpdateState`); if the node already completed init/join, re-assert
     `registerNode: false` in the kubelet config (zombie-node prevention).
   - Detect architecture, run **mlx5 rescan** (DPU NIC bring-up) unless disabled.
   - `swapoff -a`.
4. **`startSecureServer`** (goroutine):
   - Build gRPC server options: **audit-log unary interceptor** always;
     **SPIFFE mTLS creds** unless `--disable-mtls`.
   - `grpcserver.Register` installs the `NodeAgent` service **and gRPC reflection**
     (v1 + v1alpha).
   - Listen on `:15005` (default); persist `listenStatus`.
5. **`main` blocks on `ctx.Done()`** (SIGINT/SIGTERM) and exits.

The agent is a **single long-lived process per node**. Long operations run in
goroutines and report progress via persisted status rather than blocking the RPC.

---

## 6. Component architecture

### 6.1 `NodeAgentServer` (`internal/agent`)

The central type. It embeds the generated `UnimplementedNodeAgentServer` and holds:

- **Behavior seams (interfaces) for testability:**
  - `Runner nvkeutils.CommandRunner` — abstracts `exec` (`Run`, `RunSplitOutput`,
    stdin/stdout/stderr options). Mocked in tests.
  - `FileWriter nvkeutils.FileWriter` — abstracts file writes.
  - `Persister NodeStatusPersister` — load/persist `NodeStatus`.
  - `clientRegistry kubernetesClientRegistry` — lazy K8s clients.
  - `kubeadmFileOps fileOps` — ownership resolution for staged files.
- **State:** `NodeStatus *NodeStatus` guarded by `mu sync.Mutex`.
- **Concurrency guards:** `encryptionMu` (serialize encryption-config mutations),
  `operationStartMu` (serialize async op start), `nodeAgentCfgMu` (serialize
  `config.yaml` writes), plus `activeOperations`/`rewriteActive` liveness flags.
- **Config:** bundle source, index file, CNI image registry.
- **Test hooks:** `RootPath` (chroot-like prefix for file ops), poll-interval and
  timeout overrides, `apiServerReadyMaxAttempts/PollInterval`.

The RPC surface is split across many files by capability domain: `agent.go`
(kubeadm/bundles/scripts/upload core), `cni.go`, `etcd_encryption_*.go`,
`control_plane_graceful_shutdown.go`,
`patch_apiserver_manifest.go`, `preflight.go`,
`operations.go` (async framework), `agent_deb.go` (apt).

### 6.2 State & persistence (`internal/agent/status.go`)

- `NodeStatus` is a single JSON document capturing everything: name, role, phase,
  status, messages, completed phases, join info, CNI status, disk-encryption
  status, K8s API encryption status/phase, async operations map, installed
  packages, bundles/scripts history, etc.
- `JSONFilePersister` writes **atomically**: temp file → `fsync` → `rename`.
- **Snapshot discipline:** mutations take `mu`, deep-clone via `cloneNodeStatus`,
  release the lock, then persist the clone. `flushStatus` applies a snapshot to the
  live status and persists it. This avoids holding the lock across disk I/O and
  avoids aliasing bugs on slices/maps.
- Bounded history: script status is trimmed to `ScriptStatusLimit` (3).

`state.json` is the **source of truth across restarts** and is intentionally
tolerant of older schemas (normalized on load).

### 6.3 Kubernetes clients (`internal/k8s/registry.go`)

`ClientRegistry` lazily builds and caches two clients from the admin kubeconfig:

- `Client()` — typed clientset (cluster).
- `LocalClient()` — clientset pinned to `https://127.0.0.1:6443` with TLS
  verification disabled, so reload/readiness waits target *this* node's apiserver
  rather than the VIP.

Kubeconfig read and parse failures are returned with context.

### 6.4 Utilities (`pkg/`)

- `pkg/utils` — command runner, file writer, OS/arch detection, bundle
  fetch/install (HTTP + OCI), JSON/text fetch, index parsing.
- `pkg/deb` — dpkg query/inventory helpers.
- `pkg/auth` — a bearer-`TokenAuthenticator` unary interceptor (file-backed,
  hot-reloaded every 10s). *Note:* the wired-in production auth is SPIFFE mTLS in
  `main.go`; the token interceptor is an alternative/legacy auth primitive.
- `pkg/agent/proto` — generated gRPC stubs.

---

## 7. Key design patterns

These are the conventions newer code follows and that new work should adopt.

### 7.1 Async operation framework (`operations.go`)

For long-running, node-local operations (graceful shutdown, kube-vip restore, etcd
object rewrite):

- **Caller-supplied `operation_id`** is the idempotency key.
- `StartX(operation_id)` + `GetXStatus(operation_id)` pairs. Start runs the work in
  a goroutine and returns immediately; status is polled.
- Semantics:
  - Re-`Start` of an active/terminal op with the same ID/type is **idempotent**
    (returns current state; does not re-run).
  - A different non-terminal op **of the same type** is **rejected**
    (`FailedPrecondition`) — only one of a type runs at a time.
  - A persisted **non-terminal op with no live worker** (i.e. the agent restarted
    mid-op) is **rejected loudly** rather than silently resumed. Operations that
    *should* resume must be written to recover from persisted state.
- Progress is reported via a `SetStep(step)` callback persisted into
  `NodeStatus.NodeAgentOperations`.

### 7.2 Two-lane outcome contract

Disruptive, self-healing operations report **every terminal state they can leave
the node in** in the response body with a **nil gRPC error**, and reserve gRPC
error codes for "request could not be attempted, node unchanged."

Canonical example — `PatchAPIServerManifest`:
`OUTCOME_APPLIED / UNCHANGED / FAILED_RESTORED / FAILED_NOT_RESTORED`. The caller
can distinguish "we rolled you back to your pre-call state" from "we couldn't
confirm rollback, intervene" — and that distinction survives the wire. gRPC
`InvalidArgument`/`FailedPrecondition` are used only when nothing was touched.

### 7.3 Convergent / idempotent mutation

`PatchAPIServerManifest` reads the live static-pod manifest, applies
strategic-merge patches (keyed sections: volumes/mounts/env) and keyless `flag_ops`
(SetFlag/RemoveFlag on the command line) **in memory**, and writes only if the
result differs (snapshot first). A re-run against an already-applied manifest is
`OUTCOME_UNCHANGED`. Readiness against the local apiserver is verified after the
write, with automatic restore-from-snapshot on failure.

### 7.4 WAL-style resumability for encryption

`UpdateEncryptionConfig` (key rotation) writes a **write-ahead state file before
touching `encryption-config.yaml`**, then blocks until the local apiserver confirms
a reload of the new config. A `DeadlineExceeded` is explicitly **safe to retry**:
the WAL lets a resumed call avoid re-applying the mutation. This makes single-key
rotation steps crash/timeout-tolerant.

### 7.5 Multi-node encryption ordering (HA-safe)

To enable encryption on an existing cluster, initialize every control-plane node
with identity before secretbox. This order keeps new writes readable by every
control-plane API server until all nodes receive the key.

Call `PromoteKubernetesAPIEncryptionProviders` on every control-plane node to put
secretbox first. Start one rewrite with `StartEtcdEncryptionAtRestObjectsRewrite`.
Poll `GetEtcdEncryptionAtRestObjectsRewriteStatus` until the operation succeeds.
Then call `RemovePlainTextSecondaryEncryptionEntry` on every control-plane node
to remove the plaintext fallback.

Node labels (`nvke.nvidia.com/k8s-api-encryption-*`) gate progress. New clusters
enable encryption directly at `kubeadm init`.

### 7.6 Fail-fast RPC + async worker

Kubeadm init/join validate arguments **synchronously** (fail before spawning the
goroutine), set `phase=…, status=pending`, persist, then run kubeadm in a goroutine
that updates status on completion. Callers poll `GetKubeadmStatus`.

### 7.7 Staged-file boundary enforcement

Staged files are size- and path-constrained (absolute, canonical, ≤1 MiB).

---

## 8. API surface (by capability domain)

Defined in `pkg/agent/proto/node_agent.proto`. Grouped conceptually:

**Kubeadm lifecycle**
`ExecuteKubeadmInit`, `ExecuteKubeadmJoin`, `ExecuteKubeadmReset`,
`GetKubeadmStatus`, `ExecuteKubeadmPreflight`, `ExecuteNodePreflight`,
`GetKubeadmJoinString`, `UpdateAndFetchKubeAdmJoinStrings`, `GetKubeconfig`.

**CNI**
`InstallCNI` (current). Legacy in-init CNI during `ExecuteKubeadmInit` when
`no_cni_install` is false remains an internal code path only.

**Packages / bundles / repos**
`InstallPackages`, `RemovePackages`, `ListPackages`, `ConfigureAptRepo`,
`RemoveAptRepo`, `InstallBundle`, `GetBundleStatus`, `InstallCollection`.

**K8s API/etcd encryption**
`InitializeKubernetesAPIEncryption`, `PromoteKubernetesAPIEncryptionProviders`,
`UpdateEncryptionConfig`,
`StartEtcdEncryptionAtRestObjectsRewrite` + `Get…Status`,
`RemovePlainTextSecondaryEncryptionEntry`.

**Control-plane availability**
`StartK8sControlPlaneNodeGracefulShutdown` + `Get…Status`, `StartKubeVipRestore`,
`PatchAPIServerManifest`.

**Node config / ops / introspection**
`WriteNodeAgentConfig`, `ExecuteScript`, `StreamExecuteScript`, `UploadFileBundle`,
`GetSupportBundle`.

> **API convention smell / debt:** many RPCs return `status="error"` inside a
> gRPC-OK response instead of a gRPC error code (historical). Newer RPCs
> (`PatchAPIServerManifest`, encryption) use proper codes. A
> deliberate error-model convergence is a good future cleanup. `ExecuteNodePreflight`
> intentionally returns OK-with-status so orchestrators can poll "not ready yet"
> without retry storms — that one is by design.

---

## 9. Subsystem deep-dives

### 9.1 Kubeadm lifecycle
- **Init:** either full `initConfig` YAML (`kubeadm init --config`) or CLI flags
  built from `podNetworkCidr`/`controlPlaneEndpoint`/`serviceCidr`/certSANs; pins
  `--kubernetes-version` (request field → kubeadm binary version). `--upload-certs`
  always. Parses CP + worker join strings from output into state. Optionally runs
  legacy in-init CNI (see 9.2). Disables kubelet self-registration on success.
- **Join:** role inferred from `--control-plane` / `controlPlane:` in config. Writes
  encryption-config on CP joins when a key is supplied. Optional prerequisite files.
- **Reset:** `kubeadm reset -f` with optional `--skip-phases` (notably
  `remove-etcd-member` for CP teardown where the member was already removed
  cluster-side), then CNI file cleanup.
- **Preflight:** `kubeadm {init,join} phase preflight --config` with structured
  parsing of `[WARNING …]`/`[ERROR …]`/klog lines into results.
- **Zombie prevention:** `registerNode: false` written to kubelet config so a node
  restarted outside NVKE control does not re-register.

### 9.2 CNI
- **Current (`InstallCNI`):** assumes the charts/manifest **Debian package** is
  already installed on the host (verified via `dpkg`; `FailedPrecondition` if not).
  OVN resolves its chart tree from `/etc/dgx/ovn-kubernetes` or the installed
  package and `helm install`s; Calico applies operator/manifests. Image debs are
  installed separately via `InstallPackages`. `--cni-image-registry` redirects image
  pulls to a mirror for air-gap.
- **Legacy in-init CNI:** still chained from `kubeadm init`
  when `no_cni_install` is false, purely for older controller/orchestrator
  compatibility.

### 9.3 K8s API/etcd encryption — see §7.4, §7.5.
Provider: secretbox (XSalsa20-Poly1305). Object rewrite (persist under current
encryption) uses the async
`StartEtcdEncryptionAtRestObjectsRewrite` / `GetEtcdEncryptionAtRestObjectsRewriteStatus`
RPCs. All CP nodes must apply the same key.


### 9.4 Control-plane graceful shutdown & kube-vip
`runK8sControlPlaneNodeGracefulShutdown`: resolve VIP (from kube-vip manifest or
loopback) → stop kube-vip static pod (move manifest, wait for container exit via
`crictl`) → close established remote `VIP:6443` sockets (`ss -K`) → remove VIP from
`lo` → **verify a quiet window** with no new remote sockets (guards against stale
ARP/BGP-ECMP paths that would strand worker kubelets) → optionally stop kube-apiserver.
`StartKubeVipRestore` reverses only the kube-vip move (never apiserver), waits for
the VIP back on `lo`. Manifest moves are the control mechanism (kubelet reacts to
`/etc/kubernetes/manifests` changes); restore verifies backup hashes for idempotence.

### 9.5 Bundles / packages / collections
- **Bundles:** semver-versioned tgz artifacts resolved through an index
  (`ubuntu_22/24_index.json`) from S3/HTTP or OCI; installed by running an install
  command; status tracked by `request_id`.
- **apt:** `InstallPackages`/`RemovePackages`/`ListPackages` wrap apt-get + dpkg;
  `ConfigureAptRepo`/`RemoveAptRepo` manage keyrings, sources.list.d, pins.
- **Collections:** `.json` → bundles, `.txt` → apt package list; installed async,
  polled via `GetBundleStatus`.
- Direction of travel: apt/OCI over legacy bundles/profiles.

### 9.6 Support & scripts
- `GetSupportBundle` gathers `state.json`, `nvke-agent` journal, audit log, script
  log, dpkg inventory, and caller-requested absolute files into a tgz.
- `ExecuteScript`/`StreamExecuteScript` run scripts (from `/usr/local/nvke/user-scripts`
  or an uploaded path); every call is logged to the script-execution log.

---

## 10. Security model

- **Transport/authn:** SPIFFE mTLS by default. `main.go` authorizes clients whose
  SVID trust domain matches the agent's and whose SPIFFE path starts with
  `/nke-service/`. `--disable-mtls` (plaintext) is **development only**.
- **Audit:** every unary RPC is logged (method, error, user-agent, client address)
  to the audit log via an interceptor.
- **Reflection** is served on the same mTLS-protected port (prod requires valid
  SVID to enumerate/call).
- **Blast radius:** the agent runs as root and exposes `ExecuteScript` + file upload.
  This is powerful by design; the mTLS boundary + `/nke-service/` path constraint +
  audit log are the controls. Cluster-touching RPCs add their own role checks
  (`isControlPlaneRole`).
- **Secrets handling:** encryption keys and LUKS passphrases pass through RPCs; they
  are held in memory / passed via stdin to `cryptsetup`/`clevis`, not logged.

---

## 11. Deployment & packaging

- **Build:** `make build` (amd64+arm64), version injected via
  `-ldflags -X main.VERSION`. `make package` builds `.deb`s under `build/deb`.
  Protos regenerated via `make generate-protos` (Docker or `NO_DOCKER=1`), verified
  by a pre-commit hook.
- **Install:** shipped in NVKE OS images (Skysmith) or via release-bundle install
  scripts (`agent-installer`, `kubeadm-installer.sh`); runs as a systemd unit
  (`nvke-agent`). Flags supplied via `/etc/default/nvke-agent` (`AGENT_ARGS`).
- **Ports/paths:** listens on `:15005`; state at `/var/lib/nvke/state.json`; logs
  under `/var/log/`.

---

## 12. Observability

- Structured `slog` JSON everywhere (no `fmt.Printf`/`log` for new code).
- Three log streams: operational (stdout/journal), audit, script-execution.
- `logRPCSpan(ctx, name)` wraps each RPC entry/exit.
- Node-visible state: `GetKubeadmStatus`, `GetSupportBundle`, and `state.json` itself.
- On-demand `GetSupportBundle` for offline diagnosis.

---

## 13. Testing strategy

- Interface seams (`CommandRunner`, `FileWriter`, `NodeStatusPersister`,
  `kubernetesClientRegistry`, `fileOps`) enable pure unit tests with mocks.
- `RootPath` provides chroot-like isolation for file operations in tests.
- Poll-interval/timeout/attempt overrides let time-based waits run fast in tests.
- Table-driven tests with `testify/assert`; test files colocated (`*_test.go`).
- Parsers (join strings, preflight output, `ss`/`lsblk`/kube-vip manifest) are
  isolated pure functions specifically so they're unit-testable.

---

## 14. Known tech debt & evolution notes

- **Two generations of conventions.** Prefer the async-operation framework,
  two-lane outcome contract, and proper gRPC codes for new RPCs.
- **Error model inconsistency.** Legacy RPCs return `status="error"` in an OK
  response; converging on gRPC status codes (except the deliberate preflight-poll
  case) is a worthwhile cleanup.
- **Naming.** `GetKubeadmStatus` actually returns whole-node status (should be
  `GetNodeStatus`); `NodeStatus` is an ever-growing god-object.
- **Bundles → apt migration** is in progress; tgz bundles remain for some flows.
- **Dual auth primitives.** SPIFFE mTLS is wired in `main.go`; the file-backed
  token interceptor in `pkg/auth` is not the production path — clarify or remove.

---

## 15. Glossary

- **NVKE** — NVIDIA Kubernetes Engine.
- **NVKE controller** — the consolidated orchestrator in this monorepo
  (`controller/`; formerly site-workflow-engine / cloud-workflow-engine) that
  drives the agent.
- **SPIFFE/SPIRE** — workload-identity framework issuing the X.509 SVIDs used for mTLS.
- **kube-vip** — control-plane VIP provider running as a static pod.
- **static pod** — pod defined by a manifest in `/etc/kubernetes/manifests`, managed
  by kubelet; the agent manipulates these by moving manifest files.
- **bundle** — versioned tgz artifact + install command, resolved via an index.
- **collection** — a list of bundles (`.json`) or apt packages (`.txt`) to install.
- **secretbox** — XSalsa20-Poly1305 provider used for K8s API secrets encryption.

---

*Sources: `cmd/agent/main.go`, `internal/agent/*`, `internal/grpcserver/register.go`,
`internal/k8s/registry.go`, `pkg/agent/proto/node_agent.proto`, `pkg/*`, `README.md`,
`AGENTS.md`, and the `internal/README-*.md` companions.*
