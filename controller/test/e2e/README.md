# NKE End-to-End Tests

This directory contains the end-to-end (E2E) tests for NKE.

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
`nke-system/<cluster>-kubeconfig`. It supports both SPIRE exec-auth kubeconfigs
(using a JWT minted by the management cluster's SPIRE server) and ordinary
admin kubeconfigs with embedded client certificates.

### Table Tests

When you want to run the same validations across different cluster configurations, you can use table tests (`DescribeTableSubtree`).

Currently, we have tables that run against different backends (`forge-vms` and `capd`). In the future, this structure allows us to easily test across different Kubernetes versions or other infrastructure variables.

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

All HTTP GETs reach CWE through the Kubernetes API server service proxy; no
direct CWE URL is required. Access requires either a `KUBECONFIG` for the
management cluster or Kubernetes in-cluster configuration.

Run them from the repository root:

```bash
make test-health
```

Or run the e2e module directly:

```bash
cd test/e2e
go run github.com/onsi/ginkgo/v2/ginkgo@v2.27.2 run -v --label-filter=health .
```

| Variable | Description | Default |
| --- | --- | --- |
| `KUBECONFIG` | Kubeconfig used to reach the management cluster (clientcmd default loading rules). | Unset (falls back to in-cluster configuration) |
| `CWE_NAMESPACE` | Namespace containing CWE. | `cloud-workflow-engine` |
| `CWE_SERVICE` | CWE Service name used by the API server proxy. | `cloud-workflow-engine` |
| `CWE_SERVICE_PORT` | CWE Service port used by the API server proxy. | `8080` |
| `CWE_EXPECTED_VERSION` | Required CWE version; when unset, the version spec is skipped. | Unset |
| `CWE_HEALTH_TIMEOUT` | Per-spec context timeout. | `60s` |

Unfiltered runs, including `make test-e2e`, include the health specs. To exclude
them, pass `--label-filter='!health'`.

## Useful Ginkgo CLI Commands

You can run these tests with `make test-e2e`, `go test`, or the `ginkgo` CLI.

**Run via Make (recommended):**

`make test-e2e` sets a 2h suite timeout (forge-vms first boot can exceed Go's 10m
default). Pass ginkgo flags through `ARGS`:

```bash
make test-e2e
make test-e2e-happy-path
make test-e2e-capd
make test-e2e-forge-vms
make test-e2e-forge-vms-os-profiles
make test-e2e E2E_TIMEOUT=3h ARGS='--label-filter=capd'
```

### OS profile provisioning matrix

The `forge-vms && os-profiles` matrix gives each supported active OS profile an
isolated sanity lifecycle: create a one-control-plane/one-worker tenant cluster,
wait for both nodes to become Ready in CWE, and delete the cluster. The matrix
reads active profile names, architectures, and Kubernetes versions from fleet
OS profile metadata (NKX-13910). Resolution order:

1. `$CWE_PROFILES_DIR/osProfiles/metadata.yaml`
2. `$PROFILES_DIR/osProfiles/metadata.yaml`
3. sibling `../nke-site-deploy/cwe-profiles/profiles/osProfiles/metadata.yaml`

If fleet metadata is unavailable, forge-vms matrix table entries are skipped at
suite init (no panic) so CAPD and other label filters can still run.

Profiles with an embedded Kubernetes version use that version for their
cluster; version-injected profiles use the suite default.

Run the complete matrix with a longer suite timeout because entries run
sequentially:

```bash
make test-e2e-forge-vms-os-profiles
```

Every entry also carries its full profile name as a label, so a new or failing
profile can be validated independently:

```bash
make test-e2e ARGS='--label-filter="os-profiles && nke-260625-24.04-x86-fde"'
```

When adding an OS profile:

1. Add its directory and active metadata entry under
   `nke-site-deploy/cwe-profiles/profiles/osProfiles/`.
2. For an amd64 tenant profile, no separate e2e list update is required; the
   matrix includes it automatically.
3. If forge-vms cannot support it, add it to
   `forgeVMSOSProfileExclusions` in `test/e2e/os_profile_matrix_test.go` with
   the concrete reason and an owning follow-up ticket when one can remove the
   limitation.
4. Run the focused matrix test and then the new profile's label before running
   the complete matrix.

The current exclusions are:

- arm64 images because forge-vms provisions x86_64 VMs;
- the arm64 `nke-local-dev-kindest` placeholder because it is not a tenant OS
  profile; and
- legacy autoinstall profiles whose iPXE scripts boot a kernel, initrd, and ISO
  directly. forge-vms currently requires a `chain` to `qcow-imager.efi` with an
  `image_url` pointing to a qcow2 artifact.

Use the `nke-forge-vms-*` profiles for the emulator's normal control-plane and
worker defaults, or for an emulator-specific compatibility scenario such as
the old node-agent profile. Use production `nke-*` profiles as explicit worker
profiles when validating the real image, iPXE, cloud-init, or node-agent
artifacts that tenant clusters consume. The matrix uses the 2 GiB forge-vms
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
ginkgo run -v --label-filter="forge-vms" ./test/e2e
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

Run the forge-vms multi-pod-CIDR scenario and its pod-CIDR validation:

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
LOCAL_CAPD_E2E_LABEL_FILTER="oidc-enable" make test-e2e-capd-local
```

Against an already-running, OIDC-enabled CWE cluster (KUBECONFIG set):

```bash
make test-e2e ARGS='--label-filter="oidc-enable"'
```

**Trigger.** The spec enables OIDC through the canonical front door — the
`UpdateCluster{enableOidc: true}` API — which takes the `UpdateInProgress` lock and
enqueues the `EnableOIDC` workflow via the outbox, then waits for the lock to clear.

**Constructing the OIDC-off precondition.** A cluster created on an OIDC-enabled
site is always born OIDC-on (`OIDCAuthEnabled` follows the CWE `NKE_OIDC_AUTH_ENABLED`
create default), which would make `UpdateCluster{enableOidc}` a no-op — there is no
supported way to create an OIDC-off cluster on such a site. So the brownfield entry
creates its cluster with the CWE create-default flipped **off**
(`nke.SetOIDCEnabled(false)` — a live env override on the CWE deployment, leaving the
OIDC issuer/discovery/CA env intact), then flips it back **on** before enabling. This
mirrors a real brownfield cluster: one created before its site turned OIDC on. The
flag is restored in the entry's `AfterAll`; the CAPD suite runs serially so this does
not race other entries.

**Prerequisites.** The control-plane agent must be `>= v0.1.29`
(`PatchAPIServerManifest`; `EnableOIDC.Validate` fails closed below it — the CAPD
node image is pinned accordingly). The CAPD deploy stands up the OIDC substrate
(config env + SPIRE discovery provider + tenant-access controller) by default
(`NKE_OIDC_AUTH_ENABLED` / `TENANT_ACCESS_ENABLED` default true in the Tiltfile), so
the flip-on has everything it needs.

**Two ordered specs.** The first proves the enablement itself (API trigger +
kubeadm-config write-back + `nke-system` kubeconfig/trust-bundle publish); the second
asserts a SPIRE identity authenticates on the tenant apiserver. The auth half depends
on the tenant-access controller having published the SPIRE exec-auth kubeconfig +
trust bundle (`nke-system/<cluster>-kubeconfig` and the trust-bundle Secret); the
verifier pod fetches the `k8s-spiffe-workload-jwt-exec-auth` plugin itself, so no
plugin needs to be present in the test image.
