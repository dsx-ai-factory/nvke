# NVKE End-to-End Tests

This directory contains the end-to-end (E2E) tests for NVKE.

## Test Structure

The tests are designed to be environment-agnostic. They only depend on a `kubeconfig` pointing to the "site controller cluster" (also known as the management cluster) and network connectivity to its API server. By default, these tests work seamlessly with the local Tilt setup (where a local kind cluster acts as the management cluster). However, they should run against any site controller cluster as long as you provide the correct `kubeconfig` in the tests config file.

The tests are organized around the lifecycle of a tenant cluster. 

### The Main Flow

This is the most typical way of writing a test. For any given cluster configuration, the tests follow a strict sequence:

1. **Create**: Provision a tenant cluster via the CWE REST API.
2. **Validate**: Run feature validations against the live cluster (e.g. `validateOIDCPostCreate`).
3. **Delete**: Delete the tenant cluster.
4. **Post-Delete Validations**: Verify resources are properly cleaned up (e.g. `validateOIDCPostDelete`).

The all-node readiness validation uses the kubeconfig published in
`nvke-system/<cluster>-kubeconfig`. It supports both SPIRE exec-auth kubeconfigs
(using a JWT minted by the management cluster's SPIRE server) and ordinary
admin kubeconfigs with embedded client certificates.

The management namespace is per-site configurable -- the chart renders
`nvkeSystemNamespace.name` into the controller's `managementNamespace` -- and
`nvke-system` is only the default. Everything the suite reads or writes on the
site controller (published Secrets, the RBAC ConfigMap, CAPI Clusters and
Machines, verifier Pods) resolves that one namespace through
`internal/mgmtns`. The RBAC ConfigMap's own name is configurable the same way
(`rbacConfigMap.name`, `nvke-rbac` by default); `internal/oidc.ConfigMapName`
resolves it, and `NVKE_E2E_RBAC_CONFIGMAP` overrides it for a site that pinned
`nke-rbac`. Set `NVKE_E2E_MANAGEMENT_NAMESPACE` to run against a site
that pinned another -- `nke-system` for one installed before the namespace
became configurable.

### Table Tests

When you want to run the same validations across different cluster configurations, you can use table tests (`DescribeTableSubtree`).

Currently, we have tables that run against different backends (`nico-vms` and `capd`). In the future, this structure allows us to easily test across different Kubernetes versions or other infrastructure variables.

## Adding New Tests

### Adding a New Validation

Most of the time, you want to validate a new feature and do not need a specific cluster configuration. 

To add a new validation:

1. Open `test/e2e/tenant_test.go`.
2. Find the `Describe("after cluster creation", ...)` or `Describe("after cluster deletion", ...)` blocks.
3. Add a new `validateMyFeature(&test)` function call inside the appropriate block.
4. Your validation will automatically run against all backends and cluster combinations defined in the table test.

### Adding a Special Cluster Configuration

If your feature requires a specific cluster configuration (e.g., custom node pools or specific API flags):

1. Create a new `Describe` block in `tenant_test.go` or a new file.
2. Define your own cluster spec.
3. Implement the flow that fits your use case. The most common one is create -> validate -> delete.

## How the tests reach NVKE

Every NVKE call travels over a port-forward to a pod backing the
`nvke-controller` Service, built by `internal/kube.ServiceForwarder`. It
runs over the API server's own `pods/portforward` subresource, so it needs
nothing the previous Service-proxy transport did: no ingress, no NodePort, no
direct pod or Service IP reachability.

What it buys is header control. Through the Service proxy one `Authorization`
header had to satisfy two hops and the API server stripped it, so no NVKE
credential survived. Over a port-forward the kubeconfig credential
authenticates the tunnel and the request inside it is entirely the caller's.

The forward is lazy, shared process-wide, and torn down in `AfterSuite`. Its
local port is ephemeral, so parallel runs and a developer's Tilt port-forward do
not collide.

A pod replacement costs one failed request. The local listener stays up when the
pod behind it goes away, so the next request through the tunnel fails with `EOF`;
the session then ends, and the request after it rebuilds against a new Ready pod.
Specs that replace the pod themselves do not pay that, because `SetOIDCEnabled`
drops the shared forwards once the rollout completes.

## Configuration

The tests can be configured using an `e2e-config.yaml` file. A missing config file is not an error; the tests will simply use sensible defaults for a local Tilt environment. 

You can configure the following aspects of the test execution:

- `kube.kubeconfig`: Path to the kubeconfig file (defaults to standard loading rules like `$KUBECONFIG` or `~/.kube/config`).
- `kube.context`: The kubeconfig context to use (defaults to the current context).
- `cluster.capdNodeImage`: The Docker image to use for CAPD nodes (defaults to `cwe-capd-kindest-node:local`).
- `lifecycle.skipDelete`: If `true`, leaves the freshly-created cluster running after the tests finish.
- `lifecycle.preserveOnFailure`: If `true`, keeps the cluster running only if a test fails (useful for debugging).
- `lifecycle.reuseCluster`: Provide a cluster name here to skip the "Create" and "Delete" phases entirely, running the validations against an existing cluster.

## Health specs

The `health` specs are read-only smoke checks for a live CWE management-plane
deployment. They verify that:

- `/healthz` responds,
- `/readyz` responds, and
- the reported version optionally matches the expected version.

All HTTP GETs reach NVKE over the port-forward described in [How the tests reach
NVKE](#how-the-tests-reach-nvke); no direct NVKE URL is required.

Run them from the repository root:

```bash
make test-health
```

Or run the e2e module directly:

```bash
cd test/e2e
go run github.com/onsi/ginkgo/v2/ginkgo@v2.27.2 run -v --label-filter=health .
```

Only `NVKE_CONTROLLER_EXPECTED_VERSION`, `NVKE_E2E_REQUIRE_EXPECTED_VERSION` and
`NVKE_CONTROLLER_HEALTH_TIMEOUT` are specific to the health specs; the first four
apply everywhere. The `NKE_E2E_*` group configures the
credential `nvke.NewClient` discovers and presents on every ordinary NVKE call,
not the authentication specs, which build their own token per case.

| Variable | Description | Default |
| --- | --- | --- |
| `KUBECONFIG` | Kubeconfig used to reach the management cluster (clientcmd default loading rules). | Unset (falls back to in-cluster configuration) |
| `NVKE_CONTROLLER_NAMESPACE` | Namespace containing the controller. | `nvke-controller` |
| `NVKE_CONTROLLER_SERVICE` | Controller Service whose backing pod is port-forwarded to; also the name of its Deployment, container and ServiceAccount. | `nvke-controller` |
| `NVKE_CONTROLLER_SERVICE_PORT` | Controller Service port, by name or number; the pod-side port is resolved from its `targetPort`. | `8080` |
| `NVKE_CONTROLLER_EXPECTED_VERSION` | Required controller version; when unset, the version spec is skipped. | Unset |
| `NVKE_E2E_REQUIRE_EXPECTED_VERSION` | When true, an unset `NVKE_CONTROLLER_EXPECTED_VERSION` fails the version spec instead of skipping it. The validation image sets it. | Unset |
| `NVKE_CONTROLLER_HEALTH_TIMEOUT` | Per-spec context timeout. | `60s` |
| `NKE_E2E_AUTH_DISABLE` | Turns the token source off, so the suite sends no `Authorization` header. Against an enforcing NVKE every spec then fails with 401. | Unset |
| `NKE_E2E_AUDIENCE` | Overrides the audience the suite mints for. Normally discovered in `NVKE_CONTROLLER_NAMESPACE`: from `apiAuth` in the controller's settings ConfigMap (`nvke-controller-config`, or `nke-controller-config` on a site that pins the older name) when that section is enabled, otherwise from the authentication ConfigMap (`nvke-auth-config`, or `nke-auth-config`) that a controller from before `apiAuth` became its only source still has. | Discovered |
| `NKE_E2E_TOKEN_NAMESPACE` | Namespace of the service account the suite presents. | `nke-clients` |
| `NKE_E2E_TOKEN_SERVICE_ACCOUNT` | Service account the suite presents. Point this pair at `nke-outsiders`/`nke-outsider` for the 403 case. | `nke-client` |
| `NKE_E2E_TOKEN_TTL` | Minted token lifetime. The apiserver rejects anything under 10m. | `15m` |

The suite sends no token only when *neither* source turns authentication on in
that namespace: `apiAuth` is off or absent in the settings ConfigMap, and
neither `nvke-auth-config` nor `nke-auth-config` exists, which a current chart
never renders. An unauthenticated
NVKE needs no configuration here. A run against a site pinned to the older
names still discovers its audience and authenticates. A ConfigMap that exists
but cannot be read is reported rather than treated as absent, so an RBAC gap
does not quietly turn the authn specs into assertions about an unauthenticated
NVKE.

Unfiltered runs, including `make test-e2e`, include the health specs. To exclude
them, pass `--label-filter='!health'`.

## Authentication specs

Three labels, which do not all run against the same deployment.

| Label | What it covers | Deployment it needs |
|---|---|---|
| `authn` | The negative cases: no token -> 401, wrong audience -> 401, an unadmitted identity -> 403, and the anonymous routes `auth/authn/routes.go` exempts staying reachable with no credential. Also selects `authn-spire`. | Enforcing (`apiAuth.enabled` true, `apiAuth.permissive` false) |
| `authn-spire` | A SPIRE JWT-SVID end to end: a pod the spec creates fetches an SVID over the Workload API and calls NVKE with it, and with one that differs only in `aud`. | Enforcing, plus SPIFFE enabled on the NVKE pod |
| `authn-permissive` | That permissive mode counts what it would have rejected: all cases return 200 and `nvke_auth_requests_total{outcome="would_reject"}` advances. It flips the deployment itself and puts it back. | Enforcing; it arranges permissive mode for its own duration |

Each skips itself, with the reason, when the deployment is not in the state it
needs, reading only `apiAuth.enabled` and `apiAuth.permissive` from the live
NVKE's `nvke-controller-config`. Set
`NKE_E2E_REQUIRE_AUTHN=true` (as the CAPD lane does) to turn
"authentication is not deployed" from a skip into a failure; without it a
cluster with authentication off produces `SUCCESS! -- 0 Passed | 0 Failed | N
Skipped`, indistinguishable in a CI log from a suite that never had these specs.

**Prerequisites.** `tilt up` applies `deploy/k8s/nvke-auth-dev.yaml` (client
namespaces, service accounts and token-minter RBAC) and, for `authn-spire`,
`deploy/k8s/nvke-spire-client.yaml`. The configuration that admits those
identities is chart values: `hack/tilt-controller-api-auth-values.yaml` under
Tilt, and the `apiAuth` block of each e2e lane's controller overlay. NVKE's
permission to read the cluster JWKS comes from the chart
(`apiAuth.clusterIssuerDiscovery`).

`nvkectl` uses the same admitted identity (`nke-clients/nke-client`) against a
Tilt or E2E cluster. The token-minter RoleBinding in that file is for the
`nke-e2e` ServiceAccount the suite impersonates; a cluster-admin kubeconfig
(the usual Tilt user) can mint without it.

Run from the repository root:

```bash
make test-authn
make test-authn-spire
```

`hack/nvke-auth-demo.sh` drives the same four credentials with `curl` and needs
no Go at all.

## Validation image

`Dockerfile` packages the suite as an image that validates a running
management cluster, and optionally one of its tenants, from a pod inside the
management cluster, as an in-cluster test runner runs it. The image carries
every spec and runs those its two switches select: specs that install and
change nothing lasting on the cluster they check, unless
`NVKE_E2E_KEEP_ON_FAILURE=true` keeps a failed spec's objects for inspection.

| Run | `NVKE_MGMT_CLUSTER_VALIDATE` | `NVKE_TENANT_VALIDATE` | Specs labelled | Specs |
| --- | --- | --- | --- | --- |
| The management cluster (the image's default) | `true` | `false` | `mgmt-cluster-validate` | 34 |
| The management cluster and a tenant | `true` | `true` | `mgmt-cluster-validate` or `tenant-validate` | 71 |
| A tenant alone | `false` | `true` | `tenant-validate` | 40 |

Every run includes the three [health specs](#health-specs), which carry both
labels, so every run checks the controller: a tenant has none of its own. Both
`false` fails the run, which would validate nothing. An explicit
`-ginkgo.label-filter` argument overrides the switches.

### What it checks

The management cluster and a tenant share the checks below, so each runs once
per cluster. A check that does not apply to a cluster skips, saying why. An
input in the table is a [validation profile](#validation-profile) field, under
`managementCluster` or `tenant`, or an environment variable.

| # | Check | Labels | On a management cluster | On a tenant |
| --- | --- | --- | --- | --- |
| 1 | Every node Ready and schedulable | `readiness` | Always. kubeadm's control-plane taint is allowed where another node takes the work. | Always. A node may carry the taints its node pool declares and, on a control-plane node, kubeadm's; any other taint that keeps pods off fails. |
| 2 | Every Deployment, StatefulSet and DaemonSet available, and each required namespace running a ready pod | `readiness` | Always. Namespaces from `managementCluster.namespaces`. | Always. Namespaces from `tenant.namespaces`. |
| 3 | No pod failing in any namespace, and none starting to in the next 30 seconds | `readiness` | Always | Always |
| 4 | Every cert-manager Certificate Ready | `readiness` | Always | Where `tenant.components` lists `cert-manager` |
| 5 | The controller API lists clusters | `readiness` | Always | Never: a tenant runs no controller |
| 6 | Every API server `/readyz` and `/livez` check passing, etcd's included | `readiness` | Always | Always |
| 7 | `kube-scheduler` and `kube-controller-manager` Ready on every control-plane node | `readiness` | Always | Always |
| 8 | CoreDNS available | `readiness` | Always | Always |
| 9 | The API server and every kubelet at the cluster's Kubernetes version | `readiness` | Always, against `managementCluster.kubernetesVersion` or `NVKE_MGMT_CLUSTER_KUBERNETES_VERSION`; unset in both fails | Always, against the controller's record unless `NVKE_TENANT_KUBERNETES_VERSION` overrides it |
| 10 | A pod resolving a cluster DNS name | `readiness` | Always | Always |
| 11 | Argo CD running | `component` | Where `managementCluster.components` lists `argocd` | Where `tenant.components` lists `argocd` |
| 12 | Every Argo CD Application Synced and Healthy | `component` | As check 11 | As check 11 |
| 13 | The CloudNativePG operator running, every database cluster healthy | `component` | Where `managementCluster.components` lists `cnpg` | Where `tenant.components` lists `cnpg` |
| 14 | The Teleport agent running | `component` | Where `managementCluster.components` lists `teleport` | Where `tenant.components` lists `teleport` |
| 15 | cert-manager running | `component` | Where `managementCluster.components` lists `cert-manager`. Checks 2, 4 and 31 already cover it. | Where `tenant.components` lists `cert-manager` |
| 16 | kube-vip Ready on every control-plane node | `component` | Where `managementCluster.components` lists `kube-vip` | Where the controller records a control-plane VIP, or `tenant.components` lists `kube-vip` |
| 17 | Named workloads present, each with a ready pod | | Where `managementCluster.requiredWorkloads` is set | Where `tenant.requiredWorkloads` is set |
| 18 | Named workloads at their versions | | Where `managementCluster.workloadVersions` is set | Where `tenant.workloadVersions` is set |
| 19 | Every API server running the flags kubeadm-config sets, and those the run names | | Always, plus `managementCluster.apiServer.flags` | Always, plus `tenant.apiServer.flags` |
| 20 | Every API server running with an encryption provider config | | Where `managementCluster.apiServer.etcdEncrypted: true` | Where the controller records encryption at rest, or `tenant.apiServer.etcdEncrypted: true` |
| 21 | etcd running with the quota kubeadm-config sets | | Where kubeadm runs a local etcd | Where kubeadm runs a local etcd |
| 22 | Every node holding back at least the memory the run names | | Where `managementCluster.reservedMemory` is set | Where `tenant.reservedMemory` is set |
| 23 | Every StorageClass the run names present | | Where `managementCluster.storageClasses` is set | Where `tenant.storageClasses` is set |
| 24 | Anonymous access refused, and the suite's own identity held to read access | | Always; the identity part only in a pod | Always; the identity part only in a pod |
| 25 | A ready endpoint behind every admission webhook Service | | Always | Always |
| 26 | The API server's `/readyz` at kubeadm's `controlPlaneEndpoint`, from a pod | `creates` | Where kubeadm-config sets the endpoint and a node other than kube-vip's lease holder can run the probe | As on a management cluster |
| 27 | Traffic between pods, and through a Service by name and by ClusterIP, across two nodes where there are two | `creates` | Always | Always |
| 28 | A NetworkPolicy enforced | `creates` | Unless a NetworkPolicy in the probe namespace already selects the check's pods | As on a management cluster, in `tenant.namespace` |
| 29 | A StatefulSet pod resolved and reached by its headless Service name | `creates` | Always | Always |
| 30 | A PersistentVolumeClaim bound and written from a pod | `creates` | From `managementCluster.pvcStorageClass`, else the default StorageClass; skips without either | From `tenant.pvcStorageClass`, else the tenant's default StorageClass; skips without either |
| 31 | A certificate issued by a new self-signed cert-manager Issuer | `creates` | Always. With `managementCluster.certManager.approval: policy`, the request must be approved or denied instead; a denial passes, and the report says so. Where no Ready `CertificateRequestPolicy` selects the check's Issuer in the probe namespace, approver-policy would never decide, so the check creates nothing and skips, saying why. That test does not cover the RBAC binding that lets cert-manager `use` a policy: a policy that selects the Issuer but is not bound leaves the request undecided, and the check fails, naming the policies that selected it | Where `tenant.components` lists `cert-manager`, and as on a management cluster with `tenant.certManager.approval` |

A tenant also runs these, which compare it with the controller's record of it:

| # | Check | Labels | Runs |
| --- | --- | --- | --- |
| 32 | The tenant's nodes and the controller's are the same set, in the same roles, each joined, Ready and running a healthy node agent | `readiness` | Always |
| 33 | Every node carries its node pool's labels and taints | | Always |
| 34 | Every node holds back at least its pool's system and kube reserved memory | | Where a pool reserves any |
| 35 | NodeLocal DNS running on every Ready node | `readiness` | Always |
| 36 | `tenant.gpu.nodes` nodes advertise `nvidia.com/gpu`, and no node labeled `nvidia.com/gpu.present` advertises none | `gpu` | Where `tenant.gpu.nodes` is set |
| 37 | A pod given one GPU lists it with `nvidia-smi -L` | `gpu`, `creates` | Where `tenant.gpu.nodes` is above `0` |

Each of checks 1–31 also carries `site-software` or `site-distro`, and each of
checks 32–37 carries `tenant`. Any of these labels can be used in
`-ginkgo.label-filter`.

**Pods the check leaves out (check 3).** On a management cluster, the runner's own namespace,
where an earlier run's pods may be left; on a tenant, `tenant.namespace`.
Pods an earlier run's probes left, labeled `e2e.nvidia.com/validation-probe`,
are left out wherever they are.

**Anonymous access (check 24).** An anonymous request must be refused, and no
RoleBinding or ClusterRoleBinding may grant `system:anonymous` or
`system:unauthenticated` more than the public roles:
`system:public-info-viewer`, `system:service-account-issuer-discovery` and
kube-public's `kubeadm:bootstrap-signer-clusterinfo`. The suite's own identity
must be unable to read Secrets or bind roles where it is one this README
grants: on a management cluster, the image's service account, so only in a
pod; on a tenant, the provisioned ServiceAccount, wherever the suite runs. A
management cluster run outside a pod, or a lane that reaches its tenant as admin, runs as some other identity,
which is not checked. Through a proxy that demands a client certificate, such
as Teleport's, the anonymous request never reaches the API server. The
bindings, and the identity where it applies, are still checked and fail the
check on a problem; otherwise the check skips, saying anonymous access was not
checked, rather than pass.

**Encryption (check 20).** The check sees only the flag. Which provider the
config writes with is in a file on the control-plane host, so a config led by
`identity`, which stores Secrets in plain text, passes. The tenant suite's
encryption specs read a Secret from etcd to prove it; this image has no such
access.

**The endpoint probe (check 26).** The probe runs on a node other than the one
kube-vip's lease names as holder, so it crosses the network to reach the
endpoint. Where no lease names a node, the endpoint's node is unknown and the
probe may run on it, which the log says.

**The `creates` checks (26–31 and 37).** These try what workloads rely on,
with short-lived objects in the probe namespace that they delete whether they
pass or fail, unless `NVKE_E2E_KEEP_ON_FAILURE=true` keeps a failed spec's
objects. A runner that may create nothing adds
`-ginkgo.label-filter='(mgmt-cluster-validate || tenant-validate) && !creates'`. Each
object carries an `e2e.nvidia.com/site-run` label naming its run. The DNS probe
(check 10) also creates a pod, and is not labeled `creates`.

Every probe and server pod runs as the restricted PodSecurity level requires:
as user 65534, with no capabilities and the runtime's default seccomp profile.
The PVC check writes as that user through the pod's `fsGroup`, so a
StorageClass whose driver ignores `fsGroup` must create volumes that user can
write. The probe namespace must still let those pods reach one another, the API
server and the cluster DNS: a NetworkPolicy there that blocks them fails the
traffic checks.

The image carries the authentication specs, but neither switch selects them,
because they mint tokens for the scaffolding identities in
`deploy/k8s/nvke-auth-dev.yaml`. An explicit `-ginkgo.label-filter` can run
them.

### Running the image

```bash
make -C controller/test/e2e validate-image   # VALIDATE_IMAGE, VALIDATE_PLATFORM
```

The pod needs no kubeconfig: with `KUBECONFIG` unset the suite uses its service
account. That account needs:

- cluster-wide `get` and `list` on Nodes, Pods, Deployments, StatefulSets,
  DaemonSets, Jobs, StorageClasses, EndpointSlices, Validating and Mutating
  WebhookConfigurations, RoleBindings and ClusterRoleBindings,
  `cert-manager.io` Certificates, and, where the run
  lists their components, `argoproj.io` Applications and `postgresql.cnpg.io`
  Clusters; for the `creates` specs with
  `managementCluster.certManager.approval: policy`, `list` on `policy.cert-manager.io`
  CertificateRequestPolicies, and `get` on Namespaces where a policy selects
  them by label; and `get` on the non-resource URLs `/readyz`, `/livez` and
  `/version`
- in `kube-system`, `get` on ConfigMaps (`kubeadm-config`) and
  `coordination.k8s.io` Leases (kube-vip's)
- in the controller's namespace, `get` on Services and ConfigMaps (the
  authentication ConfigMap) and both `get` and `create` on
  `pods/portforward`: the port-forward tries websockets first, a `get`, and
  falls back to SPDY, a `create`
- where the controller authenticates its API, `create` on
  `serviceaccounts/token` for the identity it presents there,
  `NKE_E2E_TOKEN_NAMESPACE`/`NKE_E2E_TOKEN_SERVICE_ACCOUNT`. A controller
  is sent no token, and needs no grant, only where neither source turns
  authentication on: `apiAuth` is off or absent in its settings ConfigMap,
  and it has no authentication ConfigMap
- in the probe namespace, by default the pod's own, `create`, `get` and
  `delete` on Pods; and for the `creates` specs, the same on Services,
  StatefulSets, PersistentVolumeClaims, NetworkPolicies and `cert-manager.io`
  Issuers and Certificates, `list` on NetworkPolicies, `get` on `pods/log` and
  `get` and `delete` on Secrets (the issued certificate's); and, with
  `managementCluster.certManager.approval: policy`, `list` on `cert-manager.io`
  CertificateRequests

It needs nothing more, and should be given nothing more: the RBAC check fails
an identity that can read Secrets across the cluster or bind roles.
[`deploy/validate-site.yaml`](deploy/validate-site.yaml) grants exactly this,
and [Running it on a management cluster](#running-it-on-a-management-cluster) applies it.

### Validation image variables

Configure it with a [validation profile](#validation-profile), the
[health spec variables](#health-specs) and these:

| Variable | Purpose | Default |
| --- | --- | --- |
| `NVKE_MGMT_CLUSTER_VALIDATE` | Turns the management cluster checks on. The image sets it; elsewhere they skip. | Unset |
| `NVKE_VALIDATE_PROFILE` | Path to the [validation profile](#validation-profile), for a management cluster not laid out like `deploy/nico`. | Unset |
| `NVKE_MGMT_CLUSTER_KUBERNETES_VERSION` | The Kubernetes version the management cluster was built with, overriding `managementCluster.kubernetesVersion` for one run. Unset in both fails the version check. | The profile's |
| `NVKE_MGMT_CLUSTER_WORKLOAD_VERSIONS` | `namespace/workload=version`, comma-separated, replacing `managementCluster.workloadVersions` for one run. | The profile's |
| `NVKE_E2E_KEEP_ON_FAILURE` | `true` leaves a failed `creates` spec's objects in the probe namespace to be inspected, management cluster or tenant, and logs each one kept. The pod check leaves that namespace out, but checks 2 and 4 read the whole cluster, so a kept StatefulSet or Certificate fails the next run: delete every kept object by its `e2e.nvidia.com/site-run` label first. | Unset: deleted |

Where the controller authenticates its API, the controller API check
authenticates as `NKE_E2E_TOKEN_NAMESPACE`/`NKE_E2E_TOKEN_SERVICE_ACCOUNT`
(`auth.namespace`/`auth.serviceAccount` in a profile), whose defaults are the
development identity in `deploy/k8s/nvke-auth-dev.yaml`. A production
management cluster has no such identity: set both to one its controller
authorizes to list clusters. Where it does not, because `apiAuth` is off or
absent and there is no authentication ConfigMap, the suite sends no token.

`NVKE_CONTROLLER_NAMESPACE` (`controller.namespace`) matters wherever the
controller is not installed in `nvke-controller`, and `NVKE_CONTROLLER_SERVICE`
(`controller.service`) wherever its release is not named that. Give both
versions, in the profile or the environment: the image sets
`NVKE_E2E_REQUIRE_EXPECTED_VERSION=true`, so a run without either fails.

### Validation profile

The defaults describe a management cluster built from `deploy/nico`. A
management cluster laid out differently describes itself in a
`ValidationProfile`, and `NVKE_VALIDATE_PROFILE` points the image at it.
[`validation-profile.schema.json`](validation-profile.schema.json) is its JSON
Schema, which editors that read a `yaml-language-server` comment check as you
type. Point the comment at the schema in the release tag the image was built
from, here `v0.24.0`, so the editor checks the fields that image reads:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/dsx-ai-factory/nvke/v0.24.0/controller/test/e2e/validation-profile.schema.json
apiVersion: validate.nvke.dev/v1alpha1
kind: ValidationProfile
controller:
  namespace: nvke-controller
  service: nvke-controller
  expectedVersion: v0.24.0       # NVKE_CONTROLLER_EXPECTED_VERSION overrides it
auth:
  namespace: nvke-controller
  serviceAccount: nvke-validate
managementCluster:
  namespaces: [kube-system, cert-manager, capi-system, capnico-system, nvke-controller]
  scope: namespaces              # or cluster, the default
  components:
    - argocd
    - {name: cnpg, namespace: databases}  # where it is not in cnpg-system
  certManager: {approval: policy}
  storageClasses: [local-path]
  pvcStorageClass: local-path
  requiredWorkloads: [spire-server/spire-server]
  workloadVersions:              # NVKE_MGMT_CLUSTER_WORKLOAD_VERSIONS overrides them
    kube-system/calico-node: v3.29.1
  apiServer:
    etcdEncrypted: true
    flags:
      profiling: "false"
      tls-cipher-suites: TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
  reservedMemory: 2Gi
  kubernetesVersion: v1.34.2     # NVKE_MGMT_CLUSTER_KUBERNETES_VERSION overrides it
  probe:
    namespace: nvke-validate
    image: mirror.example/kubectl:1.34
    pullSecrets: [mirror]
tenant:
  namespace: nvke-validate
  namespaces: [kube-system, gpu-operator]
  gpu: {nodes: 4}
```

Every field is optional; one left out keeps its `deploy/nico` default.

| Field | Purpose | Default |
| --- | --- | --- |
| `controller.namespace`, `.service`, `.port` | Where the controller runs, the port by number or name. Each sets `NVKE_CONTROLLER_NAMESPACE`, `_SERVICE` or `_SERVICE_PORT`. | `nvke-controller`, `nvke-controller`, `8080` |
| `controller.healthTimeout` | How long the health specs wait, as a Go duration. Sets `NVKE_CONTROLLER_HEALTH_TIMEOUT`. | `60s` |
| `controller.expectedVersion` | The version the controller must report. | Unset: fails |
| `auth.audience`, `.namespace`, `.serviceAccount` | The identity the suite presents to the controller API. Each sets `NKE_E2E_AUDIENCE`, `NKE_E2E_TOKEN_NAMESPACE` or `NKE_E2E_TOKEN_SERVICE_ACCOUNT`. | The development identity |
| `managementCluster.namespaces` | Namespaces that must each run a ready pod, replacing the default list: `kube-system`, `cert-manager`, `capi-system` and those `deploy/nico` installs into. | The `deploy/nico` list |
| `managementCluster.scope` | `cluster` or `namespaces`. `namespaces` holds checks 2, 3, 4, 12 and 13 to `managementCluster.namespaces` and each declared component's namespace, an Argo CD Application by the namespace it deploys to. What those checks find elsewhere goes in the report and fails nothing. The other checks stay cluster-wide. See [a shared management cluster](#a-shared-management-cluster). | `cluster` |
| `managementCluster.kubernetesVersion` | The Kubernetes version the cluster was built with. | Unset: fails |
| `managementCluster.probe.namespace` | Where the probe pods and every `creates` object go. | The pod's own namespace, else `default` |
| `.probe.image` | The probe and server pods' image, which needs `sh`, `nslookup`, `wget`, `curl` and `nc`. Point it at a mirror on a cluster without internet access. | The suite's kubectl image |
| `.probe.pullSecrets` | Secrets in the probe namespace that the probe and server pods pull their image with. They run as the namespace's `default` service account, not the runner's, so a pull Secret on the runner's account does not reach them. | None |
| `.components` | Optional components the cluster runs, each a name, or `{name, namespace}` where it is not in its usual namespace: `argocd` (`argocd`), `cnpg` (`cnpg-system`), `teleport` (`teleport-agent`), `kube-vip` (`kube-system`) and `cert-manager` (`cert-manager`). A name listed twice fails. Every management cluster runs cert-manager and checks 2, 4 and 31 cover it; listing it adds check 15. | None |
| `.requiredWorkloads` | `namespace/workload`: a Deployment, StatefulSet or DaemonSet that must exist and run a ready pod. | None |
| `.workloadVersions` | `namespace/workload` to a version: a Deployment, StatefulSet or DaemonSet one of whose containers runs an image tagged with it, with or without a leading `v`. | None |
| `.apiServer.flags` | Flags every API server runs, and their values. Added to those kubeadm-config sets; a conflict between the two fails. | None |
| `.apiServer.etcdEncrypted` | `true` requires `--encryption-provider-config` on every API server. It does not show which provider the config uses. | `false` |
| `.reservedMemory` | The least memory every node holds back from pods (capacity less allocatable), such as `2Gi`. That counts the kubelet's hard eviction threshold, 100Mi by default, with its kube and system reservations: set it to their sum. | Unset |
| `.certManager.approval` | How cert-manager approves a request: `any`, where its own approver approves every one, or `policy`, where approver-policy approves only what a `CertificateRequestPolicy` allows. With `policy`, check 31 requires the approver to decide on its request, and passes on a denial, reporting it; where no policy selects its Issuer, it skips, saying why. | `any` |
| `.storageClasses` | StorageClasses that must exist. | None |
| `.pvcStorageClass` | The class the PVC check binds from. | The default class |

The fields from `.probe` down take the same form under `tenant`, which the
[tenant checks](#tenant-checks) describe.

Keep in a profile what changes rarely: where things are installed and what
the management cluster runs. The versions change with every upgrade, so the
environment overrides them for one run:

```bash
docker run --rm -v "$PWD/validation-profile.yaml:/profile.yaml:ro" \
  -e NVKE_VALIDATE_PROFILE=/profile.yaml \
  -e NVKE_CONTROLLER_EXPECTED_VERSION="$EXPECTED_VERSION" \
  -e NVKE_MGMT_CLUSTER_KUBERNETES_VERSION="$KUBERNETES_VERSION" ... "$VALIDATE_IMAGE"
```

The image runs as UID 65532, so the file must be readable by others. In a pod,
mount it from a ConfigMap, whose files are by default. The run log and the
report's validation inputs list what the profile set, and say where each
version came from:

```text
controller.expectedVersion = v0.24.0 (NVKE_CONTROLLER_EXPECTED_VERSION, overriding the profile's v0.23.1)
```

The `controller` and `auth` fields set variables the rest of the suite reads.
Where the environment sets one of those to something else, the run fails
rather than pick one:

```text
controller.namespace is "nvke-controller" in the profile but NVKE_CONTROLLER_NAMESPACE is "other-namespace"; set one
```

A profile describes the management cluster, and the checks decide what that
means for them: it names no checks to leave out. A check that does not apply
skips with its reason, which the JUnit report keeps.

The run fails before any check on a field the profile does not have, a value
of the wrong type, a second YAML document, another `apiVersion` or `kind`, or
a profile named but missing or setting nothing, naming the field:

```text
managementCluster.scope: want cluster or namespaces, got "namespace"
```

A profile cannot set the switches that turn a run on (`NVKE_MGMT_CLUSTER_VALIDATE`,
`NVKE_TENANT_VALIDATE`, `NVKE_E2E_REQUIRE_EXPECTED_VERSION`, which the image
sets) or the variables that name one tenant (`NVKE_TENANT_CLUSTER`,
`NVKE_TENANT_TOKEN_SECRET`, `NVKE_TENANT_KUBERNETES_VERSION`). It holds no
credentials and cannot turn authentication off (`NKE_E2E_AUTH_DISABLE`).

This repository ships no profiles. A management cluster's profile belongs with
the rest of that management cluster's configuration.

#### Variables the image no longer reads

Before the profile was typed, each field was a variable. The image now fails a
run that sets one, even blank, naming the field that replaced it:

```text
NVKE_SITE_COMPONENTS is no longer read: set managementCluster.components in the validation profile (NVKE_VALIDATE_PROFILE)
```

`NVKE_SITE_VALIDATE`, `NVKE_SITE_KUBERNETES_VERSION` and
`NVKE_SITE_WORKLOAD_VERSIONS` became `NVKE_MGMT_CLUSTER_VALIDATE`,
`NVKE_MGMT_CLUSTER_KUBERNETES_VERSION` and
`NVKE_MGMT_CLUSTER_WORKLOAD_VERSIONS`. Every other `NVKE_SITE_*` variable, and
each `NVKE_TENANT_*` variable for what tenants have in common, is a field:
`NVKE_SITE_PROBE_PULL_SECRETS` is `managementCluster.probe.pullSecrets` and
`NVKE_TENANT_GPU_NODES` is `tenant.gpu.nodes`. A list that was comma-separated
is a YAML list, `NVKE_SITE_COMPONENTS`' `name=namespace` is a
`{name, namespace}` entry, and
`NVKE_SITE_APISERVER_FLAGS`' semicolon-separated `name=value` is a map of
flags.

#### A shared management cluster

By default checks 2, 3 and 4 read the whole cluster, so on a management
cluster that also runs other teams' workloads, one of theirs failing fails the
run. Set `managementCluster.scope: namespaces` in that management cluster's
profile to hold those checks, and the Argo CD and CloudNativePG ones, to the
namespaces nvke installs into or depends on: `managementCluster.namespaces`
and the declared components' namespaces, plus `kube-system` and the
controller's namespace, which are always in scope. List there any namespace
nvke relies on but does not install, such as the CNI's or the one its database
clusters run in, or it goes unchecked. Problems outside the scope go in the
report as `... outside managementCluster.scope` entries.
Nodes, the API server, CoreDNS, admission webhooks and RBAC stay cluster-wide,
because a broken one breaks nvke whoever owns it.

### From a workstation

`qualify validate` runs the image with Docker against a management cluster your
workstation can reach, as one command, and summarizes the result. The checks
run as the kubeconfig's identity, not as a service account in the management cluster.

#### What you need

- A checkout of this repository, with the command run from its root.
- Go, at the version in `go.work`.
- `docker` with a running daemon, and `kubectl`, on `PATH`. To build the
  image, which is the default, also `git`, `make` and `docker buildx`; the
  build pulls its `golang` and `alpine` base images from Docker Hub and needs
  no token.
- `tsh` on `PATH` for `--run-dir` and `--teleport-cluster`.
- For `--image`, the image pulled first (`docker pull <image>`, after a
  `docker login` where the registry needs one): the run reads it from the
  local image store and does not pull it.

#### The image

Without `--image`, each run builds the image from your checkout for your
machine's architecture, tagged `nvke-validate:<git describe>`, so its checks
are the checkout's. Hold a management cluster to the checks its controller shipped with by
passing `--image` built from that release tag, as
[building the image yourself](#building-the-image-yourself) describes. No
published image exists yet. An image built from a tag before the
[validation profile](#validation-profile) was a `ValidationProfile` refuses
one, and one from before profiles ignores it.

An `--image` must be this Dockerfile's image, or one
[extended from it](#extending-the-image) that keeps its `ENV` switches and
`ENTRYPOINT`. The run relies on them:

- It mounts the kubeconfig at `/kube/config`, the profile at
  `/profile.yaml` and the run's `artifacts/` at `/artifacts`.
- It adds only `-ginkgo.label-filter` (and `-ginkgo.dry-run`), so the report
  paths must come from the image's `ENTRYPOINT`. The summary reads
  `/artifacts/junit-test-report.xml`.
- It relies on the image's `NVKE_MGMT_CLUSTER_VALIDATE=true` and
  `NVKE_E2E_REQUIRE_EXPECTED_VERSION=true`; without the second, a run with no
  expected version would skip the version check, which reads as a pass.

An image without these still runs, but reports nothing or checks less. The
container runs as your own user and group, not the image's `65532`, so it can
write `artifacts/` on your disk. It runs on the image's platform, so an
`amd64`-only image on an Apple Silicon Mac runs emulated, and much slower.

#### A management cluster kept by `qualify local`

```bash
go run ./release/cmd/qualify validate --run-dir <qualify local run dir>
```

The run directory must come from a run with `--keep-site`, whose
`node-keys/` holds one management cluster record. The node's address, login and SSH key,
its kubeconfig and both versions come from the run, so this needs no profile
and no versions. The node is reached through its jump host, so `tsh` must be
able to `tsh ssh` to the jump host as the record's login: a `tsh login` to
the Teleport cluster the jump host is enrolled in, or `TELEPORT_PROXY` and
`TELEPORT_IDENTITY_FILE`. See [reaching the jump host](#reaching-the-jump-host).
The node must present the host key the record holds, and must finish the SSH
handshake within a minute. A record from a run that recorded no host key is
refused; keep the management cluster again with a run that does.

#### A management cluster behind Teleport

```bash
tsh login --proxy=<proxy> --user=<user>
tsh kube ls                          # the name to pass
go run ./release/cmd/qualify validate --teleport-cluster <name> --profile <profile> \
  --controller-version <version> --kubernetes-version <version>
```

The run starts `tsh proxy kube` for the cluster and stops it when it ends.
Your Teleport role must give you that cluster, with Kubernetes groups that
hold at least the permissions below.

#### A management cluster any kubeconfig reaches

```bash
go run ./release/cmd/qualify validate --kubeconfig <file> [--context <name>] --profile <profile> \
  --controller-version <version> --kubernetes-version <version>
```

The image has no exec credential plugins, so the kubeconfig must carry
client certificates or a token; one that runs `tsh`, `kubelogin` or a cloud
CLI fails before any check runs, and `--teleport-cluster` is the way in for a
management cluster behind Teleport. A server on `127.0.0.1` is reached from the container: through
`host.docker.internal` under Docker Desktop, and through the host's network
on Linux.

#### Inputs

| Flag | What it is | Where it comes from |
| --- | --- | --- |
| `--profile` | The management cluster's [validation profile](#validation-profile), saying how it is laid out. Without one the checks expect a management cluster built from `deploy/nico`. | The management cluster's own configuration. This repository ships none. |
| `--controller-version` | The version the controller must report at `/api/version`, compared exactly. It overrides the profile's `controller.expectedVersion`, and is required without one. A release built at its tag reports the tag, such as `v0.23.1`; another build reports its `git describe`, such as `v0.23.1-0-g347baabc`, which its image tag does not show. | The release you deployed. To see what the controller reports: `kubectl get --raw /api/v1/namespaces/<namespace>/services/nvke-controller:8080/proxy/api/version`, with the controller's namespace, `nvke-controller` by default |
| `--kubernetes-version` | The version the management cluster was built to run, such as `v1.34.2`. It overrides the profile's `managementCluster.kubernetesVersion`, and is required without one. | The management cluster's build record, not the API server: check 9 compares the API server and every kubelet with it. |
| `--full` | Also run the `creates` checks: 26–31, and 37 on a tenant. | |
| `--only <expr>` | A Ginkgo label expression the run is narrowed to, such as `readiness`. | |
| `--dry-run` | List what would run, and run nothing. | |
| `--image` | Run this image instead of building one. | See [the image](#the-image). |
| `--out` | Where the run's files go. | Default: `validate-<time>` under `--run-dir`, else `nvke/validate/<cluster>-<time>` under your user cache directory (`~/Library/Caches` on macOS, `~/.cache` on Linux). |

Any `NVKE_*` or `NKE_E2E_*` variable in your environment reaches the image
as it is. One the profile also sets fails the run where the two differ,
except a version, which overrides the profile's for one run. That is how a
run adds [a tenant](#tenant-checks) (`NVKE_TENANT_VALIDATE=true`
and `NVKE_TENANT_CLUSTER`) and how it names the identity the controller API
check presents where the controller authenticates
(`NKE_E2E_TOKEN_NAMESPACE`, `NKE_E2E_TOKEN_SERVICE_ACCOUNT`).
[Running it on a management cluster](#running-it-on-a-management-cluster) shows how to tell whether it
does. `--controller-version`, `--kubernetes-version` and `--profile` set
their own variables, so the environment's values of those are not passed.
The run refuses to start where `NVKE_MGMT_CLUSTER_VALIDATE`,
`NVKE_E2E_REQUIRE_EXPECTED_VERSION` or `NKE_E2E_AUTH_DISABLE` is set, even to
nothing: the image sets them, and changing one can turn every check off.

#### Credentials

The kubeconfig's identity needs the permissions
[running the image](#running-the-image) lists for the service account. In a
read-only run, that includes creating and deleting the DNS probe pod in the
probe namespace. From a workstation that namespace is `default` unless
`managementCluster.probe.namespace` names another. Where the controller
authenticates its API, the identity also needs `create` on
`serviceaccounts/token` for the `NKE_E2E_TOKEN_*` service account.

That identity is what keeps the run from changing the management cluster. The label
filter is not a guard, as the Dockerfile notes. In a pod, the narrow service
account of [running the image](#running-the-image) is. From a workstation,
it is your kubeconfig's identity. An admin identity works, but then the
filter `qualify validate` sets is all that holds the run to the specs it
selects. `--only` narrows that filter and cannot widen it. On a shared management cluster,
use an identity bound to `deploy/validate-site.yaml`'s ClusterRole and Roles
rather than an admin one. Check 24 tests the runner's own identity only in a
pod, so an admin identity does not fail it from a workstation.

The only credential the run writes is the kubeconfig it hands the
container, kept in the run directory with mode `0600` and deleted when the
run ends.

#### What a run leaves

The run prints its directory first. That directory holds `artifacts/`, with
the JUnit and JSON reports, plus `run.log`, `inputs.env` (the flags and the
names of the variables passed, not their values), a copy of the profile and,
for `--run-dir` and `--teleport-cluster`, `connect.log`. The run ends with
the counts, every failure, the reason for each skip, and what a check
reported without failing, such as a scoped check's findings outside its
scope. It exits non-zero when a check fails, and when no check passed, as
where every one skipped or `--only` selected none. An interrupt reaches the
suite, which deletes what it made on the management cluster before it exits.

### Tenant checks

With `NVKE_TENANT_VALIDATE=true`, the image also validates the tenant
`NVKE_TENANT_CLUSTER` names, found by name through the controller API, with
checks 1–37 in [What it checks](#what-it-checks). Turning the tenant checks on
without naming a tenant fails the run rather than skipping them.

The suite reaches the tenant as a ServiceAccount the runner provisions in it,
NVKE's [tenant automation](../../../docs/tenant-clusters/kubernetes-api-access.md)
path, and never reads the kubeconfig the controller publishes, which with OIDC
off is an admin credential. In the tenant, that ServiceAccount needs:

- the cluster-wide reads the management cluster checks need, listed above,
  without the controller's namespace or the token request
- in `tenant.namespace`, the rights the probe namespace needs on the
  management cluster

On the management cluster, the suite's own account also needs `get` on the
Secret named by
`NVKE_TENANT_TOKEN_SECRET`, in its own namespace. The runner writes the
ServiceAccount's token to it under `token` and the tenant's CA under `ca.crt`,
as a `kubernetes.io/service-account-token` Secret in the tenant carries them,
and the API server URL under `server` where the tenant has no control-plane
VIP or is reached some other way. Without `server`, the suite uses
`https://<control-plane VIP>:6443`.

| Variable | Purpose | Default |
| --- | --- | --- |
| `NVKE_TENANT_CLUSTER` | The tenant to validate, by name. Required when `NVKE_TENANT_VALIDATE` is true. | Unset |
| `NVKE_TENANT_TOKEN_SECRET` | The Secret, in the suite's namespace on the management cluster, that holds the tenant credential. | `nvke-validate-<cluster>` |
| `NVKE_TENANT_KUBERNETES_VERSION` | Overrides the version the controller records. The profile has no field for it: the controller records each tenant's own, so there is nothing to describe. | The record's |
| `NVKE_TENANT_WORKLOAD_VERSIONS` | `namespace/workload=version`, comma-separated, replacing `tenant.workloadVersions` for one run. | The profile's |

What the tenants have in common is in the profile's `tenant` section. It
describes every tenant a run is pointed at alike, so tenants built
differently, such as GPU and CPU-only ones, each take their own profile, and
the runner passes the one that fits the tenant `NVKE_TENANT_CLUSTER` names:

| Field | Purpose | Default |
| --- | --- | --- |
| `tenant.namespace` | The tenant namespace the probe pods and every `creates` object go in. Its pods are left out of the pod check. | `nvke-validate` |
| `tenant.namespaces` | Namespaces that must each run a ready pod. | `kube-system` |
| `tenant.probe.image`, `.probe.pullSecrets` | As under `managementCluster`, in `tenant.namespace`. The GPU pod pulls with the Secrets too. | The suite's kubectl image, none |
| `tenant.gpu.nodes` | How many nodes must advertise GPUs. | Unset |
| `tenant.gpu.image` | The GPU pod's image, which needs `sh`; the NVIDIA container runtime supplies `nvidia-smi`. | `nvcr.io/nvidia/cuda:12.9.1-base-ubuntu24.04` |

The rest mirror the management cluster's: `components`, `certManager`,
`storageClasses`, `pvcStorageClass`, `requiredWorkloads`, `workloadVersions`,
`apiServer` and `reservedMemory` take the same form under `tenant`, and are
listed against checks 11–23, 30 and 31 in [What it checks](#what-it-checks).
One differs: `tenant.pvcStorageClass` picks the StorageClass check 30 claims
from, and nothing else; check 23 reads `tenant.storageClasses`.

The image is not published. A management cluster builds it from its controller's release
tag, as [Building the image yourself](#building-the-image-yourself)
describes, so the tests it runs are the tests that shipped with its
controller.

The entrypoint sets `-ginkgo.timeout=3h`. A healthy cluster finishes far sooner,
but on a broken one each check waits out its own limit, and a suite timeout
reports the checks it did not reach as interrupted. A runner that restates the
entrypoint should keep the flag, and its own step timeout should be longer.

Results go to `/artifacts`, which the runner mounts:

| File | Contents |
| --- | --- |
| `junit-test-report.xml` | The selected specs only (`-e2e.junit-report`). Ginkgo's own JUnit report lists every spec the filter left out as skipped. A spec that failed on a Gomega assertion about an error carries that error's text as a `failure-cause` property, JSON-encoded and cut at a word to at most 4 KiB. |
| `report.json` | Ginkgo's JSON report, every spec. Its report entries record the inputs each validation resolved and, for a tenant, the cluster ID and API server it reached, never the credential; a spec that failed on a Gomega assertion about an error also records that error's text, which its failure message already carries. |

A failure says why where the cluster records it: a pod that never ran gives
its scheduling condition, each container's waiting reason and its latest
Warning events; a workload its failing conditions; a crash-looping container
its restarts and last exit; a node its Ready condition. Events need `list`
on events in the probe namespace, which the RBAC above does not grant; without
it they are left out and the rest stands. A request the API server refused
says so and points here.

To run it by hand against the current kube context, as your own user so the
container can read your kubeconfig and write `artifacts/`:

```bash
mkdir -p artifacts
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/artifacts:/artifacts" -v "$HOME/.kube/config:/kube/config:ro" \
  -e KUBECONFIG=/kube/config -e NVKE_CONTROLLER_EXPECTED_VERSION=v0.23.1 \
  -e NVKE_MGMT_CLUSTER_KUBERNETES_VERSION=v1.34.2 nvke-validate:local
```

### Building the image yourself

This is how a site gets the image. The build is meant to be reproducible: the
base images are pinned by digest, and the timestamps, version and revision come
from the commit, so the same commit built with the same arguments should give
the same digest. CI checks this on every change by building it twice on one
runner, the second time with no cache and every file's mtime changed. It does
not compare builds across machines or BuildKit versions.

Build from the release tag of the controller the site runs, the same version
the site pins: `v<version>`, or `controller/v<version>` for a controller
released before one `vX.Y.Z` tag released every component. Never build from
`main`: its suite tests the next controller, not the one installed. The
Dockerfile first shipped in `v0.23.0`, and in `controller/v0.3.0` the same
day; earlier tags have no image to build.

`validate-image-oci` and `validate-image-push` build for two platforms and
write an OCI archive or push, which Docker's default `docker` builder driver
cannot do. First create a builder that runs BuildKit in its own container, as
CI does; otherwise the build fails with "OCI exporter is not supported for the
docker driver". This is needed once per machine.

```bash
docker buildx create --name nvke --driver docker-container --bootstrap --use
```

```bash
git clone --branch v0.23.1 --depth 1 https://github.com/dsx-ai-factory/nvke.git nvke-v0.23.1
cd nvke-v0.23.1
make -C controller/test/e2e validate-image-oci VALIDATE_VERSION=v0.23.1
cat controller/test/e2e/bin/validate-image-digest.txt
```

`validate-image-push VALIDATE_REPO=<repository> VALIDATE_PUSH_TAGS=v0.23.1`
pushes it to the management cluster's own registry instead, as `<repository>:v0.23.1`.
`<repository>` is the whole repository path, such as
`registry.example.com/nvke-validate`. Either target derives the rest from the
checkout.

Without a checkout, BuildKit can take the tag as a remote context. Then the
arguments make derives must be given by hand, from the tagged commit:
`SOURCE_DATE_EPOCH` is `git log -1 --format=%ct`, `VALIDATE_REVISION` the full
commit hash. Where cloning the repository needs authentication, give BuildKit
a token with read access as the `GIT_AUTH_TOKEN` secret; otherwise drop the
`--secret` line.

```bash
docker buildx build -f controller/test/e2e/Dockerfile \
  --platform linux/amd64,linux/arm64 --provenance=false --sbom=false \
  --build-arg SOURCE_DATE_EPOCH=<commit time> \
  --build-arg VALIDATE_VERSION=v0.23.1 --build-arg VALIDATE_REVISION=<commit> \
  --secret id=GIT_AUTH_TOKEN,env=GIT_AUTH_TOKEN \
  --output type=image,name=<repository>:v0.23.1,push=true,oci-mediatypes=true,rewrite-timestamp=true \
  'https://github.com/dsx-ai-factory/nvke.git#v0.23.1'
```

Arguments that change where things come from, not what, keep the digest:

| Argument | Make variable | Use |
| --- | --- | --- |
| `GOPROXY` | `VALIDATE_GOPROXY` | A Go module mirror. `go.sum` still decides the modules. |
| `GOLANG_IMAGE`, `ALPINE_IMAGE` | (`--build-arg` in `VALIDATE_BUILDX_ARGS`) | A mirror of the base images. Keep the `@sha256:` digests from the Dockerfile; another digest is another image. |

Any other change, such as the platforms or attestations added with
`--provenance` or `--sbom`, gives a different digest.

### Keeping a management cluster's image current

A management cluster needs one image per controller version it runs, not the
newest one.
Build each release tag, `vX.Y.Z` or `vX.Y.Z-rc.N`, when it appears, and push
it to the management cluster's registry under that version. The nightly `vX.Y.Z-beta.N` tags
are not releases:

```bash
git ls-remote --tags --refs https://github.com/dsx-ai-factory/nvke.git 'v*' | grep -v -- '-beta\.'
ver=v0.23.1   # a tag the management cluster's registry does not have yet
git clone --branch "${ver}" --depth 1 https://github.com/dsx-ai-factory/nvke.git "nvke-${ver}"
make -C "nvke-${ver}/controller/test/e2e" validate-image-push VALIDATE_VERSION="${ver}" \
  VALIDATE_REPO=<repository> VALIDATE_PUSH_TAGS="${ver}"
```

Each tag gets its own checkout, so building the next one never meets the last
one's directory.

A run picks the image whose tag is the management cluster's controller
version and sets `NVKE_CONTROLLER_EXPECTED_VERSION` to the image's tag. The
image requires a version, so a run against a management cluster whose
controller is another version fails the version check rather than passing on
the wrong tests. Upgrading the controller means running the next tag.

Rebuilding a tag on the same builder gives the same digest, so a retried job
pushes the same image again. A job can also rebuild a tag it has already
pushed and compare the digests, to check that nothing in its own build path
changed.

### Running it on a management cluster

[`deploy/validate-site.yaml`](deploy/validate-site.yaml) runs the management
cluster checks as a Job in an `nvke-validate` namespace, with the RBAC above, and keeps the
results on a claim. To run the image from a workstation instead, see
[from a workstation](#from-a-workstation).

The Job runs every `mgmt-cluster-validate` check, the `creates` ones included. For a
run that creates nothing, give its container
`args: ["-ginkgo.label-filter=mgmt-cluster-validate && !creates"]`; the `ENTRYPOINT`
keeps its report paths ahead of them. On a management cluster other teams
share, set `managementCluster.scope` as
[a shared management cluster](#a-shared-management-cluster) describes.

Release tags up to `v0.23.1` have no `controller/test/e2e/deploy/`. Where the
management cluster's tag has it, use the manifests from the checkout the image was built
from; otherwise take them from `main`, keeping the image built from the
release tag. The image from such a tag reads the management cluster's
Kubernetes version from `NVKE_SITE_KUBERNETES_VERSION` and labels its checks
`site-validate`, so set `old=1` below to rename the variable in the manifest,
and write `site-validate` for `mgmt-cluster-validate` in a label filter. Run
these from the directory that holds the checkout, with the kube context
pointing at the management cluster:

```bash
ver=v0.23.1                                 # the management cluster's controller version
image=<repository>:${ver}                   # pushed from ${ver} above
git clone --depth 1 https://github.com/dsx-ai-factory/nvke.git nvke-main   # only for a tag without deploy/
deploy=nvke-main/controller/test/e2e/deploy   # or nvke-${ver}/controller/test/e2e/deploy
old=1                                       # only for a tag without deploy/; unset otherwise
k8s=<version>                               # the Kubernetes version the cluster is meant to run
read -r ctrl_ns ctrl < <(kubectl get deploy -A -l app.kubernetes.io/name=nvke-controller \
  -o jsonpath='{.items[0].metadata.namespace} {.items[0].metadata.name}')
token_ns=<namespace> token_sa=<service account>   # see below
fill() {
  sed -e "s|__IMAGE__|${image}|g" -e "s|__CONTROLLER_VERSION__|${ver}|g" \
    -e "s|__KUBERNETES_VERSION__|${k8s}|g" -e "s|__CONTROLLER_NAMESPACE__|${ctrl_ns}|g" \
    -e "s|__CONTROLLER_NAME__|${ctrl}|g" \
    -e "s|__TOKEN_NAMESPACE__|${token_ns}|g" -e "s|__TOKEN_SERVICE_ACCOUNT__|${token_sa}|g" \
    ${old:+-e "s|NVKE_MGMT_CLUSTER_KUBERNETES_VERSION|NVKE_SITE_KUBERNETES_VERSION|"} \
    "${deploy}/$1"
}
fill validate-site-token.yaml | kubectl apply -f -   # only if the controller authenticates
fill validate-site.yaml | kubectl apply -f -
```

The chart names the controller's Deployment after the Helm release, so it is
found by its `app.kubernetes.io/name` label, and `fill` hands its name to the
suite; a release that sets `nameOverride` changes that label too. The
controller's image tag gives `ver`:
`kubectl -n "${ctrl_ns}" get deploy "${ctrl}" -o jsonpath='{.spec.template.spec.containers[0].image}'`.
The version check compares `ver` exactly with what the controller reports,
`kubectl get --raw "/api/v1/namespaces/${ctrl_ns}/services/${ctrl}:8080/proxy/api/version"`.
A release built at its tag reports the tag. A controller that reports more,
such as `v0.23.1-0-g347baabc`, fails the check against its image tag, so put
what it should report in the Job's `NVKE_CONTROLLER_EXPECTED_VERSION`.
`k8s` is the version the cluster was built to run, from its build record
rather than the cluster: check 9 compares the API server and every kubelet
with it, so a version read from the API server would pass a cluster running
the wrong one.

`token_ns`/`token_sa` name the identity the suite presents to the controller
API, one the controller authorizes to list clusters; `validate-site-token.yaml`
lets the Job mint its token. The controller authenticates exactly when its
Deployment mounts an `nvke-auth-config` volume, whatever the ConfigMap behind
it is called. This prints that ConfigMap's name, and nothing where there is
none; an error means the check did not run, not that there is no
authentication:

```bash
kubectl -n "${ctrl_ns}" get deploy "${ctrl}" \
  -o jsonpath='{.spec.template.spec.volumes[?(@.name=="nvke-auth-config")].configMap.name}'
```

Apply `validate-site-token.yaml` if it prints a name. The suite finds the
audience itself where that name is `nvke-auth-config` or `nke-auth-config`, or
where the controller's settings ConfigMap holds the `apiAuth` section. For a
ConfigMap with another name, managed outside the chart, add
`NKE_E2E_AUDIENCE` to the Job's `env`, set to one of the `audiences` it lists.

Where the registry needs credentials, add `imagePullSecrets` to the Job and
the artifacts pod. Describe a management cluster not laid out like
`deploy/nico` in a [validation profile](#validation-profile): put it in a
ConfigMap before applying the Job, and uncomment the Job's profile volume,
mount and `NVKE_VALIDATE_PROFILE`:

```bash
kubectl create namespace nvke-validate --dry-run=client -o yaml | kubectl apply -f -
kubectl -n nvke-validate create configmap nvke-validate-profile \
  --from-file=profile.yaml=<profile>
```

The Job's `env` always sets both versions, `NVKE_CONTROLLER_NAMESPACE`,
`NVKE_CONTROLLER_SERVICE`, `NKE_E2E_TOKEN_NAMESPACE` and
`NKE_E2E_TOKEN_SERVICE_ACCOUNT`, blank where `fill` had nothing for them. A
blank one leaves the profile's value. A version set there overrides the
profile's; any of the others set there to something other than the profile's
fails the run. Set each in one place.

Three of the profile's fields need more RBAC than `validate-site.yaml` grants
by default: listing `argocd` or `cnpg` in `managementCluster.components` needs the
commented Application or Cluster rule in its ClusterRole uncommented,
`managementCluster.certManager.approval: policy` needs the commented
CertificateRequest rule in `nvke-validate-probe` and the commented
CertificateRequestPolicy rule, with the Namespace one where a policy selects by
label, in the ClusterRole uncommented, and setting
`managementCluster.probe.namespace` needs the `nvke-validate-probe` Role and
RoleBinding moved to that namespace.

Follow the run, then read the Job's conditions. `Complete` means no selected
spec failed. Some may still have skipped, saying why, where an input they
need is unset: the reports list them, so read those before calling the
management cluster validated. `Failed` means a spec failed, or the Job ended without finishing
the suite, such as a pod that never started or a run past the Job's deadline;
the logs and `kubectl -n nvke-validate describe job nvke-validate`
say which. Neither means it is still running. Recent Kubernetes versions also
list `SuccessCriteriaMet` or `FailureTarget` alongside.

```bash
kubectl -n nvke-validate logs -f job/nvke-validate
kubectl -n nvke-validate get job nvke-validate \
  -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{"\n"}{end}'
```

Copy the reports out through a pod that mounts the claim:

```bash
fill validate-site-artifacts.yaml | kubectl apply -f -
kubectl -n nvke-validate wait --for=condition=Ready pod/nvke-validate-artifacts --timeout=5m
kubectl -n nvke-validate cp nvke-validate-artifacts:/artifacts ./artifacts
kubectl -n nvke-validate delete pod nvke-validate-artifacts
```

`artifacts/junit-test-report.xml` lists each selected spec, and why any failed
or skipped.
To run again, delete the Job and the claim, so no report from the last run
survives, then apply `validate-site.yaml` again:

```bash
kubectl -n nvke-validate delete job nvke-validate --wait
kubectl -n nvke-validate delete pvc nvke-validate-artifacts --wait
fill validate-site.yaml | kubectl apply -f -
```

A run that finds no reports on the claim never started the suite; the Job's
events say why. To remove everything, `fill` each file into
`kubectl delete -f -`.

This validates the management cluster only. Validating a tenant as well needs the runner to
provision the tenant credential first, as [Tenant checks](#tenant-checks)
describes.

### Extending the image

To add tools, certificates or wrapper scripts, start from the image built from
the release tag rather than editing this Dockerfile, so the suite stays the one
that shipped:

```dockerfile
FROM <repository>:v0.23.1
USER 0
RUN apk add --no-cache curl
USER 65532:65532
```

Keep the user, the `ENV` switches, and `ENTRYPOINT` or restate it in full:
the report paths a runner reads are in it. Specs cannot be added this
way; they are compiled into the test binary.

## Useful Ginkgo CLI Commands

You can run these tests with `make test-e2e`, `go test`, or the `ginkgo` CLI.

**Run via Make (recommended):**

`make test-e2e` sets a 2h suite timeout (nico-vms first boot can exceed Go's 10m
default). Pass ginkgo flags through `ARGS`:

```bash
make test-e2e
make test-e2e-core-capd
make test-e2e-core-nico-containers
make test-e2e-happy-path
make test-e2e-capd
make test-e2e-nico-vms
make test-e2e-nico-vms-releases
make test-e2e E2E_TIMEOUT=3h ARGS='--label-filter=capd'
```

The two `core` targets run one control-plane node and one worker through create,
tenant-node readiness, delete, and tenant-access cleanup. Pull-request mirror
branches use these rows for bounded feedback. Runs on `main`, scheduled runs,
and manual runs keep the comprehensive backend rows, including the extended
lifecycle validations.

The CAPD `cp-controller` rows advertise failure domains through
`DockerCluster.spec.failureDomains` and check that control-plane Machines land
only in eligible domains and stay balanced through create, scale, rolling
update, and delete. These rows reject `lifecycle.reuseCluster`, since each
needs its own set of domains.

To run only these scenarios locally:

```bash
make test-e2e ARGS='--label-filter="capd && cp-controller"'
```

### Local CAPD e2e (`make test-e2e-capd-local`)

`make test-e2e-capd-local` (Colima + kind + Tilt + CAPD) runs the CAPD suite
against a controller that provisions every control plane from the cluster-level
`controlPlaneConfig`, cp-controller entries included. It skips the `nvkectl` row,
which needs the CLI and `kubectl` that the local runner does not have, so the
local run is not CLI coverage. An entry that describes its control plane as one
node pool is sent as the equivalent `controlPlaneConfig`.

The CAPD node image uses Open vSwitch's kernel datapath. Geneve leaves through
the kernel route on Docker's uplink, and `br-ex` stays a dummy gateway so OVN
does not adopt that interface. `NKE_OVS_DATAPATH_TYPE=netdev` selects the
userspace datapath instead. The existing DNS workload probe and networking
scenarios exercise this setup.

To iterate faster, run a single filter instead:

```bash
LOCAL_NKE_E2E_LABEL_FILTER="capd && cp-controller" make test-e2e-capd-local
```

### Release provisioning matrix

The `nico-vms && releases` matrix gives each supported active release an
isolated sanity lifecycle: create a one-control-plane/one-worker tenant cluster,
wait for both nodes to become Ready in CWE, and delete the cluster. The matrix
reads active release names, architectures, and Kubernetes versions from the
fleet releases (NodePoolReleases that are not deprecated, and the
MachineImages they reference), which is maintained outside this repository.
If `NVKE_CONTROLLER_RELEASES_DIR` is set, it is used exclusively and must
contain at least one YAML release manifest. A missing or empty explicit
directory causes suite initialization to fail.

When the variable is unset, releases are resolved in this order:

1. sibling `../nke-site-deploy/cwe-profiles/releases`
   (an NVIDIA-internal fleet repository; external contributors use the
   environment variable above instead)
2. the stub mirror in `test/fixtures/fleet-releases`

If neither fallback contains releases, nico-vms matrix table entries are
skipped at suite init so CAPD and other label filters can still run.

Profiles with an embedded Kubernetes version use that version for their
cluster; version-injected profiles use the suite default.

Run the complete matrix with a longer suite timeout because entries run
sequentially:

```bash
make test-e2e-nico-vms-releases
```

Every entry also carries its full release name as a label, so a new or failing
release can be validated independently:

```bash
make test-e2e ARGS='--label-filter="releases && nke-260625-24.04-x86-fde"'
```

When adding a release:

1. Add its MachineImage, ClusterRelease and NodePoolRelease to the fleet
   releases (for NVIDIA maintainers, the internal fleet repository; otherwise
   the directory `$NVKE_CONTROLLER_RELEASES_DIR` points at).
2. For a tenant profile, no separate e2e list update is required; the matrix
   includes it automatically on a runner of its architecture.
3. If nico-vms cannot support it, add it to
   `nicoVMSReleaseExclusions` in `test/e2e/release_matrix_test.go` with
   the concrete reason and an owning follow-up ticket when one can remove the
   limitation.
4. Run the focused matrix test and then the new profile's label before running
   the complete matrix.

The matrix holds the active profiles built for the runner's own architecture.
nico-sim takes the guest architecture from the host it runs on, never from the
profile, so an amd64 runner boots the amd64 profiles and an arm64 runner boots
the arm64 ones. A profile for the other architecture is left out without an
exclusion entry.

`nicoVMSReleaseExclusions` holds the profiles no runner can boot:

- the arm64 `nvke-local-dev-kindest` placeholder, because it is not a tenant OS
  profile; and
- legacy autoinstall profiles whose iPXE scripts boot a kernel, initrd, and ISO
  directly. nico-vms currently requires a `chain` to `qcow-imager.efi` with an
  `image_url` pointing to a qcow2 artifact.

Use the `nvke-nico-vms-*` profiles for the emulator's normal control-plane and
worker defaults, or for an emulator-specific compatibility scenario such as
the old node-agent profile. Use production `nke-*` profiles as explicit worker
profiles when validating the real image, iPXE, cloud-init, or node-agent
artifacts that tenant clusters consume. The matrix uses the 2 GiB nico-vms
instance type with explicit kube and system memory reservations.

The happy-path targets run create/readiness/delete for the selected backend(s)
and intentionally exclude scale, upgrade, and reboot validations
(`!scale && !upgrade && !reboot`) to keep the common development loop fast.
Run those validations explicitly when needed:

```bash
make test-e2e ARGS='--label-filter="happy-path && scale"'
make test-e2e ARGS='--label-filter="happy-path && upgrade"'
make test-e2e ARGS='--label-filter="happy-path && reboot"'
make test-e2e ARGS='--label-filter="happy-path && (scale || upgrade || reboot)"'
```

**Run all tests:**

```bash
ginkgo run -v ./test/e2e
```

**Filter by backend:**
Run tests only for a specific backend using labels:

```bash
ginkgo run -v --label-filter="capd" ./test/e2e
ginkgo run -v --label-filter="nico-vms" ./test/e2e
```

**Filter by validation:**
Run only specific validations, such as OIDC:

```bash
ginkgo run -v --label-filter="spire-oidc || lifecycle || readiness" ./test/e2e
```

You usually need to include `lifecycle` when filtering validation specs. Some
post-create validations, such as all-node readiness, use their own feature label
(`readiness`) rather than `lifecycle`, so include that label too if you want the
complete happy path. This is a side effect of running create and delete cluster
operations as specs: if you only run specs with `spire-oidc` then creating and
deleting the cluster get skipped, which means you never get a cluster you can
validate. Moreover, there is no way to mark a spec as "always run regardless of
the label filters".

**Combine filters (e.g. run only OIDC validations on CAPD):**
You can combine labels to be highly specific. For example, to run only the OIDC validations against the CAPD backend:

```bash
ginkgo run -v --label-filter="capd && (spire-oidc || lifecycle || readiness)" ./test/e2e
```

Run the nico-vms multi-pod-CIDR scenario and its pod-CIDR validation:

```bash
ginkgo run -v --label-filter="multi-pod-cidrs && (pod-cidr || lifecycle)" ./test/e2e
```

## Brownfield OIDC enablement e2e

`validateBrownfieldOIDC` (label `oidc-enable`) creates an OIDC-off cluster and turns
OIDC on for it through the **production front door** — the
`UpdateCluster{enableOidc: true}` API — then asserts the auth-config write-back
landed in `kube-system/kubeadm-config` (the `authentication-config` arg and the
`oidc-auth-config` volume) and that a SPIRE identity authenticates on the tenant
apiserver.

Full local run (Colima + kind + Tilt + CAPD), only this spec:

```bash
# from the repo root, with an agent checkout at ../agent
LOCAL_NKE_E2E_LABEL_FILTER="oidc-enable" make test-e2e-capd-local
```

Against an already-running, OIDC-enabled CWE cluster (KUBECONFIG set):

```bash
make test-e2e ARGS='--label-filter="oidc-enable"'
```

**Trigger.** The spec enables OIDC through the canonical front door — the
`UpdateCluster{enableOidc: true}` API — which takes the `UpdateInProgress` lock and
enqueues the `EnableOIDC` workflow via the outbox, then waits for the lock to clear.

**Constructing the OIDC-off precondition.** A cluster created on an OIDC-enabled
site is always born OIDC-on (`OIDCAuthEnabled` follows
`oidc.managementOIDCAuth.enabled` in `nvke-controller-config`), which would make `UpdateCluster{enableOidc}` a no-op — there is no
supported way to create an OIDC-off cluster on such a site. So the brownfield entry
creates its cluster with the CWE create-default flipped **off**
(`nvke.SetOIDCEnabled(false)` — a live edit of `nvke-controller-config` and a controller
restart, leaving the OIDC issuer/discovery/CA settings intact), then flips it back **on** before enabling. This
mirrors a real brownfield cluster: one created before its site turned OIDC on. The
flag is restored in the entry's `AfterAll`; the CAPD suite runs serially so this does
not race other entries.

**Prerequisites.** The control-plane agent must be `>= v0.1.29`
(`PatchAPIServerManifest`; `EnableOIDC.Validate` fails closed below it — the CAPD
node image is pinned accordingly). The CAPD deploy stands up the OIDC substrate
(the `oidc.managementOIDCAuth` settings + SPIRE discovery provider + tenant-access controller) by default
(`NKE_OIDC_AUTH_ENABLED` / `TENANT_ACCESS_ENABLED` default true in the Tiltfile), so
the flip-on has everything it needs.

**Two ordered specs.** The first proves the enablement itself (API trigger +
kubeadm-config write-back + `nvke-system` kubeconfig/trust-bundle publish); the second
asserts a SPIRE identity authenticates on the tenant apiserver. The auth half depends
on the tenant-access controller having published the SPIRE exec-auth kubeconfig +
trust bundle (`nvke-system/<cluster>-kubeconfig` and the trust-bundle Secret); the
verifier pod fetches the `k8s-spiffe-workload-jwt-exec-auth` plugin itself, so no
plugin needs to be present in the test image.

## Hardware qualification

`qualify_e2e_test.go` (label `site-controller`) builds its own site controller
on a Forge node and runs the `nico-forge` tenant against it, instead of using the
management cluster in the kube context. `-e2e.site-stage` picks how far it goes;
without it the lane is skipped:

- `provision`: provision the node over the site's jump host and check its egress.
- `bootstrap`: also run `nvkectl bootstrap site` and wait for the API server.
- `deployment`: also deploy the SBOM stack, wait for CWE, and check that NKTP
  delivers the site's telemetry (below). Takes one machine; no tenant is built.
- `full`: also create and delete a tenant.

At `full` the lane runs the [validation image](#validation-image)'s site checks,
1–31 in [What it checks](#what-it-checks), against the site it built. The
`nico-forge` tenant, in either lane, runs checks 1–37 before it is deleted,
reached as the tenant's admin rather than a provisioned ServiceAccount. Its
probe pods run in `nvke-validate`, which the lane creates if it is missing
and, on a tenant that outlives the run, removes again. The site lane builds a
new management cluster, so it refuses `lifecycle.reuseCluster`. The
lane takes the Kubernetes version from the spec it built. For the management cluster it
lists what it installed: `cert-manager` as a component (check 15) and the
stack's default StorageClass (check 23). It sets no other component,
workload, flag, memory, storage or GPU input, for the management cluster or the tenant, so
the checks that wait on one of those skip there. The tenant's expectations from its
controller record still apply: kube-vip (check 16) where the record has a
control-plane VIP, and encryption (check 20) where it records encryption at
rest. Each set stops at its first failure, so
a broken site does not wait out every later check.

In either lane the tenant gets one worker, or N with `-e2e.tenant-workers=N`,
and the site lane's capacity check asks for the same N.

`-e2e.tenant-gpu-runtime`, off by default, gives the tenant a GPU runtime so
checks 36 and 37 run there too. Before the tenant checks, two specs labeled
`gpu` and `gpu-runtime` install the runtime with [AICR](https://github.com/NVIDIA/aicr)
and then run AICR's deployment checks against it. The checks' results go in the
spec's report entry. The tenant checks then expect every worker to advertise
`nvidia.com/gpu`. The recipe is AICR's NVKE Ubuntu training recipe for the
run's `gpu` capability ([setup guide](https://github.com/NVIDIA/aicr/blob/main/docs/integrator/nvke-gb200-setup.md)),
without its InfiniBand fabric: [`aicr/`](aicr/) holds the overlay, passed with
`--data`, that turns off `network-operator`, `sriov-ib-networks` and
`rdma-modules`. The tenant has one GPU worker and no RDMA checks, and the
fabric needs ConnectX-7 firmware and partition setup the lane has not
qualified. The install needs `aicr`, `helm` and
`kubectl` on `PATH`, plus `timeout` or `gtimeout`: AICR's nvsentinel install
will not apply its CRDs without one, and macOS has neither (`brew install
coreutils` provides `gtimeout`). It reaches the tenant as its admin through the
lane's relay. `aicr` is pinned in `make/tool-versions.mk`. A run with no `gpu`, a
`gpu` that has no recipe (only `gb200` has one), or an `ubuntu` capability other
than `24.04` gets one skipped spec saying which, and the GPU checks stay off.
Neither spec runs once an earlier check on the tenant failed. In the
`qualify-e2e` workflow the switch is the `tenant-gpu-runtime` dispatch input or
the `QUALIFY_E2E_TENANT_GPU_RUNTIME` repository variable. Either one installs the
three tools and adds 65 minutes to each clock. `qualify local` takes the same
switch as `--tenant-gpu-runtime`, and checks the three tools and a `timeout`
are on `PATH` before it takes a machine.

Both lanes create the same `nico-forge` tenant and both entries carry that
label, so pick between them with `site-controller`, which only the lane that
builds a site carries. At `full` the site lane also registers one tenant entry
per dialect, so the filter has to exclude the ones belonging to other
providers by name. Measured against this tree with `-ginkgo.dry-run` and
`-e2e.site-stage=full`:

| label filter | specs | which |
|---|---|---|
| `nico-forge && !site-controller` | 42 | the tenant, against the site controller in the kube context |
| `site-controller && !nico-dsx` | 80 | the lane that builds a site, its own tenant included |
| `nico-forge` | 84 | both |
| `site-controller` | 122 | the site lane carrying every dialect's tenant, and so the foreign one that fails the run |

`-e2e.tenant-gpu-runtime` adds one spec to each lane's tenant, or two with
`-e2e.capabilities=gpu=gb200,ubuntu=24.04`.

`-e2e.site-stage`, not the label, is what decides whether a machine is
provisioned: the site lane's tenant entries are built only when it is set, so
a bare `nico-forge` without it selects 42 specs and reaches no hardware.

The node is torn down afterwards (tenants are deleted first). `-e2e.keep-site`, or
`lifecycle.preserveOnFailure` with a failed spec, keeps it. `lifecycle.skipDelete`
keeps every tenant as well, passed or not, and skips the spec that deletes one;
`qualify local --keep-tenant` sets it, and the workflow never does.
`lifecycle.nodeKeyDir` writes the site node's SSH key there, so a kept node can
be logged into after the run; `qualify local` sets it with either keep flag.
A kept management cluster's admin kubeconfig goes there too, as `<node>.kubeconfig`, aimed at
`127.0.0.1` on the node's API server port, so it works once that port is
forwarded to the node's loopback through the jump host. A kept tenant's
kubeconfig, as the controller publishes it, goes there as `<cluster>.kubeconfig`.
A kept management cluster's `<node>.json` records how it is reached: its address, login,
jump host and kubeconfig, with no credential. All are mode 0600.

To run the [validation image](#running-the-image) against a kept management cluster, run
`qualify validate --run-dir` on the run, as [From a workstation](#from-a-workstation)
shows. It reads the node's record, forwards its API server port through the
jump host, and takes the two versions from the run's `steps.env`. A lane management cluster
is laid out like `deploy/nico`, so its profile needs only what the lane
declares: `cert-manager` in `managementCluster.components` and its
StorageClass in `managementCluster.storageClasses`.

Prerequisites, all read from the shell that runs the suite. Neither the proxy
nor the jump host nor any identifier is checked in, and secrets are read only
from the environment; the lane's maintainers can give you the values.

- **A Teleport login** to the cluster the jump host is enrolled in, current for
  the whole run: a `full` run takes over an hour, and its teardown still dials
  the node. See [Reaching the jump host](#reaching-the-jump-host).
- **The Forge site.** The suite reads the API key, and `environments/forge.yaml`
  reads the rest. An unset one of these fails the run before a machine exists:

  | Name | What it is |
  |---|---|
  | `FORGE_API_KEY` | The Forge API key. Secret. An environment in another org names its own variable with `apiKeyEnv`, and `forge-perf` reads `FORGE_PERF_API_KEY`. |
  | `FORGE_ORG_ID` | The org the machines are created in |
  | `FORGE_SITE_ID`, `FORGE_VPC_ID`, `FORGE_SUBNET_ID` | Site-1, and the VPC and subnet its machines attach to |
  | `FORGE_SSH_KEY_GROUP_ID` | Site-1's key group, for serial console access |
  | `FORGE_INSTANCE_TYPE_CPU_ID`, `FORGE_INSTANCE_TYPE_GPU_ID` | Site-1's CPU and GPU (L40S) machine types |

  These default to empty, and a site run stops on an empty jump host:

  | Name | What it is |
  |---|---|
  | `FORGE_JUMP_HOST` | The jump host in front of site-1's machines |
  | `FORGE_TELEPORT_LOGIN` | Your login on it, one of the Logins `tsh status` lists. Empty is tsh's default. |

  A GB200 stream runs at site-2, which is skipped unless all of its
  `FORGE_SITE_2_*` names in `environments/forge.yaml` are set.
- **A registry login** for what `site-inputs` pulls: capi-provisioned unless
  `SITE_PROVISIONED_BUILD=1`, and the controller or nvkectl when they are not
  built. GHCR is the default,
  and its packages are private, so `docker login ghcr.io` with a token that
  can read them; NVCR is below.
- **The versions** `-e2e.versions` names. `qualify plan`, run from the
  repository root, resolves them the way a qualification run does (see
  [Running one](../../../docs/qualification/overview.md#running-one)):

  ```bash
  go run ./release/cmd/qualify plan --subject nvke --scope l40s --output plan.json
  ```

**On DSX** (`-e2e.environment=dsx`) the credential is different and the stage
is limited. Each datahall carries its own SSA client, named for the key it is
filed under in `environments/dsx.yaml`:

```bash
export SSA_ISSUER=…                                  # issuer BASE url; the
export DSX_DATAHALL_1_SSA_CLIENT_ID=…         # suite appends /token
export DSX_DATAHALL_1_SSA_CLIENT_SECRET=…     # 15-minute TTL
export DSX_DATAHALL_1_JUMP_HOST=…             # the rest are identifiers
export DSX_DATAHALL_1_API=…                   # required; the datahall's host
export DSX_DATAHALL_1_SITE_ID=… DSX_DATAHALL_1_TENANT_ID=…
export DSX_DATAHALL_1_VPC_ID=… DSX_DATAHALL_1_VPC_PREFIX_ID=…
export DSX_DATAHALL_1_SSH_KEY_GROUP_ID=… DSX_DATAHALL_1_INSTANCE_TYPE_ID=…
```

There is no cluster fallback: an unset client fails the run naming both
variables. The names derive from the datahall key, so a credential scoped to
one datahall cannot be picked up by a run against another. A datahall that
declares `issuerPath` mints on its own API host and needs no `SSA_ISSUER`, and
one served under a private CA takes the PEM in `DSX_<DATAHALL>_CA_CERT`.

Every stage runs, `full` included: the datahall has its own tenant spec, so a
`full` run builds the site and then a control plane and a worker on it. On a
FLAT (`autoNetwork`) datahall the tenant's machines attach by autoNetwork too,
and a private CA reaches CAPNICo as `ca.crt` in `nico-credentials`. The
site lane registers one tenant entry per dialect and each decodes the
environment strictly, so a hand-run selects its own with
`-ginkgo.label-filter="site-controller && !nico-forge"`; the workflow does the
same from the provider it resolved.

The DSX datahall is **amd64 only**, which is what an absent `arch` capability
already defaults to, so the capability below is explicit rather than required.
Stage amd64 OS images to match:

```bash
go test -timeout 5h . -args \
  -ginkgo.label-filter="site-controller && !nico-forge" -ginkgo.v \
  -e2e.config=e2e-config.yaml -e2e.environment=dsx -e2e.site-stage=provision \
  -e2e.capabilities=arch=amd64 \
  -e2e.versions=controller=<…>,os-image=<amd64 url>,os-image-bootloader=<amd64 url>,…
```

**On Forge** (`-e2e.environment=forge`), build the inputs, then run. With the
`site-inputs` defaults no `site:` block is needed at all, and
`SITE_PROVISIONED_VERSION` has to be the same capi-provisioned version as
`-e2e.versions` names:

```bash
gmake site-inputs SITE_NVKE_CONTROLLER_VERSION=$(git rev-parse --short=12 HEAD) SITE_PROVISIONED_VERSION=<version>
go test -timeout 5h . -args \
  -ginkgo.label-filter="site-controller && !nico-dsx" -ginkgo.v \
  -e2e.config=e2e-config.yaml -e2e.environment=forge -e2e.site-stage=full \
  -e2e.versions=controller=<the same value>,os-image-bootloader=<url>,os-image=<url>,capi-provisioned=<version>,kube=<version>,cni-ovn=<version>,kind=<version>,clusterctl=<version>,nerdctl=<version>
```

A Forge run requires every one of those versions and fails before a machine
exists when one is missing, naming the ones it did not get. The suite carries no
default for them. A qualification run gets them from the plan, and the tool
versions (`kind`, `clusterctl` and `nerdctl`) are the pins in
`make/tool-versions.mk`.

The two OS-image keys may carry an architecture, as `os-image-arm64`. That is
how one run boots two: a GB200 tenant puts arm64 workers behind a control plane
of whatever `-e2e.capabilities` asked for under `arch`, and each machine takes
the pair built for it. The unsuffixed keys above are what a run on a single
architecture passes, and are used for any architecture that has no suffixed
pair. The CI workflow always passes suffixed pairs, from the
`QUALIFY_E2E_OS_IMAGE_<ARCH>` and `QUALIFY_E2E_OS_IMAGE_BOOTLOADER_<ARCH>`
repository variables.

`SITE_NVKE_CONTROLLER_VERSION` and `-e2e.versions=controller=` have to be the same
string, because they name one image: the archive `site-inputs` writes, and the
reference the suite hands the bootstrap and the deploy path, which the node
resolves out of its own containerd rather than a registry. The suite builds
that reference from `site.images.controller.repository` and the version, so a
repository here has to match the one `site-inputs` pulled or tagged.

The published images come from GHCR by default. To pull them from NVCR
instead, log in with an NGC key and point `site-inputs` and the config at the
NGC org; the image names are the same:

```bash
echo "$NGC_KEY" | docker login nvcr.io -u '$oauthtoken' --password-stdin
gmake site-inputs SITE_IMAGE_REGISTRY=nvcr.io/<org> SITE_NVKE_CONTROLLER_VERSION=...
# and in e2e-config.yaml, site.images.{controller,capi-provisioned}.repository
# under nvcr.io/<org>
```

The node still pulls CAPNICo from GHCR either way: its release asset names a
GHCR image, and nothing else on the node pulls at all.

`site-inputs` builds that image from the checkout. nvkectl is built here too and
is a client of the CWE it bootstraps, so the two have to be one commit; the
newest CWE on GHCR predates #850 and rejects this checkout's nvkectl at
"creating the site cluster". `SITE_NVKE_CONTROLLER_BUILD=0` pulls
`SITE_NVKE_CONTROLLER_VERSION` as a published tag instead, for qualifying a release
once one is cut past #850. The controller is built for `SITE_ARCH`, either
architecture, cross-compiled on the host's.

capi-provisioned is pulled by default. `SITE_PROVISIONED_BUILD=1` builds it from
the checkout for `SITE_ARCH` instead, tagged and versioned
`SITE_PROVISIONED_VERSION`, which is then what `-e2e.versions=capi-provisioned=`
has to say. With all three built, nothing in `site-inputs` pulls, and
`SITE_IMAGE_REGISTRY` is only the prefix the images are named under:

```bash
gmake site-inputs SITE_IMAGE_REGISTRY=localhost/nvke \
  SITE_NVKE_CONTROLLER_VERSION=<commit> SITE_PROVISIONED_BUILD=1 SITE_PROVISIONED_VERSION=<commit>
```

`qualify local --from-source` does this, with the commit under test as each
version.

To stage a published nvkectl instead, set `SITE_CLI_BUILD=0` and name the
artifact. `site-inputs` pulls `nvkectl-linux-$(SITE_ARCH)` out of it into
`$(SITE_INPUTS)/nvkectl/` and, when the binary is for the host it runs on,
checks it reports `SITE_CLI_VERSION`. The pull checks each blob against the
manifest the registry served for that tag, which catches a corrupted transfer
but does not pin the tag.

nvkectl is published per alias and platform, so the reference is
`<registry>/nvkectl:<alias>-linux-<arch>`, where the alias is a version for a
release and `sha-<commit>` for a commit between releases. It needs `oras` and a
`docker login` to the registry:

```bash
gmake site-inputs SITE_CLI_BUILD=0 SITE_NVKE_CONTROLLER_VERSION=... \
  SITE_CLI_VERSION=v0.1.0 SITE_CLI_REF=nvcr.io/<org>/nvkectl:v0.1.0-linux-amd64
# and in e2e-config.yaml, site.nvkectl: <SITE_INPUTS>/nvkectl/nvkectl-linux-<arch>
# and on the suite, -e2e.versions=...,cli=<version>
```

The Forge workflow does all of this when given a `cli-version` input or the
`QUALIFY_E2E_CLI_VERSION` repository variable, and stops if the binary does not
report the version it was asked for. Pick a CLI and a controller that agree on
the cluster API; the pairing is not checked until the bootstrap reaches
"creating the site cluster".

The deploy path is exported from the controller's own commit, tracked files
only, so nothing in the working tree reaches the node. It carries the
controller's chart and overlay, so it follows `-e2e.versions=controller=`: that
commit when the controller was built from one, else its `controller/<version>`
release tag, which has to be fetched in a shallow clone. Set
`site.deployPathRef` only when developing the deploy path itself.

### NKTP on the built site

The `full` stage checks that NKTP delivers the site's telemetry by querying its
backend, not by reading Pod status: a Collector can be Ready and export nothing.
`internal/nktp` runs the checks NKTP's own Kind lane makes on any cluster, a
span it pushes through the node Collector itself, and what only a site
wires up: the control plane, controller, Temporal and SPIRE metrics and logs,
the API server's audit log, a trace of a request the spec itself makes to the
controller, and the site's name on every signal.
`siteTelemetry` in `site_nktp_test.go` lists the site-specific checks by
component, one line per expectation.

The checks run in two stages, each polling its checks concurrently. First
every signal has to arrive, within eight minutes. Then the checks that nothing
bad arrives, which pass trivially on an empty backend, have three minutes to
hold and must keep holding for one more. The spec therefore takes the slowest
check of each stage in turn: a few minutes when healthy, and at most about
twelve when a signal never arrives. The failure names every check that did not
hold and what it last saw.

### Reaching the jump host

Teleport is the intended route. Locally that is a `tsh login` to the cluster
the jump host is enrolled in. In CI it is a bot identity in `TELEPORT_PROXY`
and `TELEPORT_IDENTITY_FILE`, which
`.github/workflows/qualify-e2e.yml` obtains by joining with the
repository's GitHub OIDC token — nothing is stored. The bot's login is `root`,
so the workflow sets `FORGE_TELEPORT_LOGIN=root` and
`FORGE_SITE_2_TELEPORT_LOGIN=root`, which each site's `teleportLogin` in
`environments/forge.yaml` reads. Unset is tsh's default.

For a site whose jump host is *not* enrolled, set `JUMP_HOST_PASSWORD` and the
harness logs into it over plain SSH and asks its sshd to forward, instead of
shelling out to `tsh`. The jump host needs `AllowTcpForwarding` on; the error
names it if not. Set the site's `jumpHostUser` (defaults to its
`teleportLogin`) and `jumpHostKeyFingerprint` in its environment file; the
fingerprint comes from:

```bash
ssh-keyscan -t ed25519 <jump host> | ssh-keygen -lf -
```

Without the fingerprint the run still works and warns on every dial that it
accepted the key unchecked -- which means it offered the password to whatever
answered on port 22. The password is the jump host's alone: the node beyond it
is reached with the ed25519 key the run generates, never with a password.

This route is for a future site, and the CI lane no longer offers it. There is
no network path from the ARC fleet to the site's jump host on port 22 at all —
only the Teleport proxy reaches it — so `qualify-e2e.yml` always
goes through `tsh`.

The `node-agent-upgrade` label selects the controller-driven agent tests. These
tests use real agent bundles and an isolated bundle mirror. They cover upgrades,
downgrades, restoration after a failed installation, automatic retries, target
changes, process restarts, deferred joins, and deletion drains. The tests also
check the agent version before kubeadm and reject TLS clients without an identity.
Workers keep the version baked into the image.

To include the fixtures in a local run, set
`LOCAL_NVKE_E2E_NODE_AGENT_UPGRADE=true`. Use a separate checkout at
`92a3039bae7a12d10244263bff80776d83c2fe72` for the image's old agent. Set
`NODE_AGENT_REPO_PATH` to that checkout's `agent/` directory and
`NODE_AGENT_VERSION=v0.1.990`. The fixtures for the desired versions use the
current checkout. Choose unique names for the management cluster, node image,
and BGP container:

```bash
KIND_CLUSTER_NAME=nvke-agent-upgrade-e2e \
CAPD_NODE_IMAGE=nvke-agent-upgrade-node:local \
CAPD_KINDEST_NODE_IMAGE=nvke-agent-upgrade-node:local \
BGP_CONTAINER_NAME=nvke-agent-upgrade-bgp \
NODE_AGENT_REPO_PATH=/path/to/baseline/agent \
NODE_AGENT_VERSION=v0.1.990 \
LOCAL_NVKE_E2E_NODE_AGENT_UPGRADE=true \
LOCAL_NKE_E2E_LABEL_FILTER=node-agent-upgrade \
flox activate -- bash controller/hack/local-backend-e2e.sh
```

If the Docker host already runs, set `LOCAL_NVKE_E2E_REUSE_DOCKER_HOST=true`
to reuse it without changing the host configuration. The test repairs only its
private fixture artifact. It does not change a released bundle.

## OIDC acceptance

From `controller/`, run `make test-oidc-reference` to check NVKE's rendered
authentication config and RBAC against the pinned envtest API server.
`make test-oidc-acceptance-live` verifies authentication and provider snapshot
preservation during control-plane joins through CAPD/Tilt; successful runs delete
their management Kind cluster, and failed runs retain it and artifacts for
inspection. After changing agent or bundle sources, remove the cached CAPD image
and CNI packages from `controller/` before rerunning, and delete retained
management clusters when finished:

```bash
docker rmi cwe-capd-kindest-node:oidc-acceptance-jointoken
rm -rf .local/cni
kind get clusters
kind delete cluster --name "nke-oidc-live-<run_id>"
```
