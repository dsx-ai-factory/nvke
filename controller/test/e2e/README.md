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
site controller (published Secrets, the `nke-rbac` ConfigMap, CAPI Clusters and
Machines, verifier Pods) resolves that one namespace through
`internal/mgmtns`. Set `NVKE_E2E_MANAGEMENT_NAMESPACE` to run against a site
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
| `NVKE_CONTROLLER_NAMESPACE` | Namespace containing the controller. The old names `NKE_CONTROLLER_NAMESPACE` and `CWE_NAMESPACE` are deprecated and still read when this is unset, until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes them. | `nvke-controller` |
| `NVKE_CONTROLLER_SERVICE` | Controller Service whose backing pod is port-forwarded to; also the name of its Deployment, container and ServiceAccount. The old names `NKE_CONTROLLER_SERVICE` and `CWE_SERVICE` are deprecated and still read when this is unset, until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes them. | `nvke-controller` |
| `NVKE_CONTROLLER_SERVICE_PORT` | Controller Service port, by name or number; the pod-side port is resolved from its `targetPort`. The old names `NKE_CONTROLLER_SERVICE_PORT` and `CWE_SERVICE_PORT` are deprecated and still read when this is unset, until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes them. | `8080` |
| `NVKE_CONTROLLER_EXPECTED_VERSION` | Required controller version; when unset, the version spec is skipped. The old names `NKE_CONTROLLER_EXPECTED_VERSION` and `CWE_EXPECTED_VERSION` are deprecated and still read when this is unset, until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes them. | Unset |
| `NVKE_E2E_REQUIRE_EXPECTED_VERSION` | When true, an unset `NVKE_CONTROLLER_EXPECTED_VERSION` fails the version spec instead of skipping it. The validation image sets it. | Unset |
| `NVKE_CONTROLLER_HEALTH_TIMEOUT` | Per-spec context timeout. The old names `NKE_CONTROLLER_HEALTH_TIMEOUT` and `CWE_HEALTH_TIMEOUT` are deprecated and still read when this is unset, until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes them. | `60s` |
| `NKE_E2E_AUTH_DISABLE` | Turns the token source off, so the suite sends no `Authorization` header. Against an enforcing NVKE every spec then fails with 401. | Unset |
| `NKE_E2E_AUDIENCE` | Overrides the audience the suite mints for. Normally discovered from the authentication ConfigMap in `NVKE_CONTROLLER_NAMESPACE` -- `nvke-auth-config`, or `nke-auth-config` on a site that pins the older name. | Discovered |
| `NKE_E2E_TOKEN_NAMESPACE` | Namespace of the service account the suite presents. | `nke-clients` |
| `NKE_E2E_TOKEN_SERVICE_ACCOUNT` | Service account the suite presents. Point this pair at `nke-outsiders`/`nke-outsider` for the 403 case. | `nke-client` |
| `NKE_E2E_TOKEN_TTL` | Minted token lifetime. The apiserver rejects anything under 10m. | `15m` |

The suite sends no token only when *neither* `nvke-auth-config` nor
`nke-auth-config` exists in that namespace, so an unauthenticated NVKE needs no
configuration here. A run against a site pinned to the older name still
discovers its audience and authenticates. A ConfigMap that exists but cannot be
read is reported rather than treated as absent, so an RBAC gap does not quietly
turn the authn specs into assertions about an unauthenticated NVKE.

Unfiltered runs, including `make test-e2e`, include the health specs. To exclude
them, pass `--label-filter='!health'`.

## Authentication specs

Three labels, which do not all run against the same deployment.

| Label | What it covers | Deployment it needs |
|---|---|---|
| `authn` | The negative cases: no token -> 401, wrong audience -> 401, an unadmitted identity -> 403, and the anonymous routes `auth/authn/routes.go` exempts staying reachable with no credential. Also selects `authn-spire`. | Enforcing (`AUTH_CONFIG_PATH` set, `permissiveMode` false) |
| `authn-spire` | A SPIRE JWT-SVID end to end: a pod the spec creates fetches an SVID over the Workload API and calls NVKE with it, and with one that differs only in `aud`. | Enforcing, plus SPIFFE enabled on the NVKE pod |
| `authn-permissive` | That permissive mode counts what it would have rejected: all cases return 200 and `nvke_auth_requests_total{outcome="would_reject"}` advances. It flips the deployment itself and puts it back. | Enforcing; it arranges permissive mode for its own duration |

Each skips itself, with the reason, when the deployment is not in the state it
needs, reading `AUTH_CONFIG_PATH` off the live NVKE Deployment and
`permissiveMode` from its `nke-controller-config`. Set `NKE_E2E_REQUIRE_AUTHN=true` (as the CAPD lane does) to turn
"authentication is not deployed" from a skip into a failure; without it a
cluster with authentication off produces `SUCCESS! -- 0 Passed | 0 Failed | N
Skipped`, indistinguishable in a CI log from a suite that never had these specs.

**Prerequisites.** `tilt up` applies `deploy/k8s/nvke-auth-dev.yaml` (client
namespaces, service accounts, token-minter RBAC and the `nvke-auth-config`
ConfigMap) and, for `authn-spire`, `deploy/k8s/nvke-spire-client.yaml`. NVKE's
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

`Dockerfile` packages the suite as an image that validates a running site from
a pod inside its cluster, as an in-cluster test runner runs it. The image carries
every spec and runs those its two switches select: specs that create, change
and install nothing on the site. Today that is the health specs. The
authentication specs are not, because they mint tokens for the scaffolding
identities in `deploy/k8s/nvke-auth-dev.yaml`.

| Run | `NVKE_SITE_VALIDATE` | `NVKE_TENANT_VALIDATE` | Specs labelled |
| --- | --- | --- | --- |
| The site (the image's default) | `true` | `false` | `site-validate` |
| The site and a tenant | `true` | `true` | `site-validate` or `tenant-validate` |
| A tenant alone | `false` | `true` | `tenant-validate` |

The health specs carry both labels, so every run checks the controller: a
tenant has none of its own. Both `false` fails the run, which would validate
nothing. An explicit `-ginkgo.label-filter` argument overrides the switches.

```bash
make -C controller/test/e2e validate-image   # VALIDATE_IMAGE, VALIDATE_PLATFORM
```

The pod needs no kubeconfig: with `KUBECONFIG` unset the suite uses its service
account. In the controller's namespace that account needs `get` on Services,
`list` on Pods and both `get` and `create` on `pods/portforward`, and nothing
else: the port-forward tries websockets first, a `get`, and falls back to SPDY,
a `create`. Configure it with
the [health spec variables](#health-specs); `NVKE_CONTROLLER_NAMESPACE` matters
wherever the controller is not installed in `nke-controller`, and
`NVKE_CONTROLLER_SERVICE` wherever its release is not named that. Set
`NVKE_CONTROLLER_EXPECTED_VERSION`: the image sets
`NVKE_E2E_REQUIRE_EXPECTED_VERSION=true`, so a run without it fails.

Each controller release publishes the image built from the same commit, as
`nvke-validate:v<version>` beside `nvke-controller` (see
`.github/workflows/image-validate-nvcr.yml`), so the tests a site runs are the
tests that shipped with its controller.

Results go to `/artifacts`, which the runner mounts:

| File | Contents |
| --- | --- |
| `junit-test-report.xml` | The selected specs only (`-e2e.junit-report`). Ginkgo's own JUnit report lists every spec the filter left out as skipped. |
| `report.json` | Ginkgo's JSON report, every spec. |

To run it by hand against the current kube context, as your own user so the
container can read your kubeconfig and write `artifacts/`:

```bash
mkdir -p artifacts
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/artifacts:/artifacts" -v "$HOME/.kube/config:/kube/config:ro" \
  -e KUBECONFIG=/kube/config -e NVKE_CONTROLLER_EXPECTED_VERSION=v0.1.1 nvke-validate:local
```

### Building the image yourself

The build is reproducible: the same commit built with the same arguments gives
the published image's digest, so a site can build it in its own pipeline and
still check it against the release. CI proves this on every change by
building it twice, the second time with no cache and every file's mtime
changed.

Build from the release tag of the controller the site runs,
`controller/v<version>`, the same version the site pins. Never build from
`main`: its suite tests the next controller, not the one installed.

```bash
git clone --branch controller/v0.2.0 --depth 1 https://github.com/dsx-ai-factory/nvke.git
cd nvke
make -C controller/test/e2e validate-image-oci VALIDATE_VERSION=v0.2.0
cat controller/test/e2e/bin/validate-image-digest.txt
```

`validate-image-push VALIDATE_REPO=<repository> VALIDATE_PUSH_TAGS=v0.2.0`
pushes it instead. Either target derives the rest from the checkout. Compare
the digest with the published one; when they match, the release's build
provenance covers this build too:

```bash
crane digest nvcr.io/<dsx-org-team>/nvke-validate:v0.2.0
gh attestation verify oci://nvcr.io/<dsx-org-team>/nvke-validate:v0.2.0 --repo dsx-ai-factory/nvke \
  --signer-workflow dsx-ai-factory/nvke/.github/workflows/image-validate-nvcr.yml
```

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
  --build-arg VALIDATE_VERSION=v0.2.0 --build-arg VALIDATE_REVISION=<commit> \
  --secret id=GIT_AUTH_TOKEN,env=GIT_AUTH_TOKEN \
  --output type=image,name=<repository>:v0.2.0,push=true,oci-mediatypes=true,rewrite-timestamp=true \
  'https://github.com/dsx-ai-factory/nvke.git#controller/v0.2.0'
```

Arguments that change where things come from, not what, keep the digest:

| Argument | Make variable | Use |
| --- | --- | --- |
| `GOPROXY` | `VALIDATE_GOPROXY` | A Go module mirror. `go.sum` still decides the modules. |
| `GOLANG_IMAGE`, `ALPINE_IMAGE` | (`--build-arg` in `VALIDATE_BUILDX_ARGS`) | A mirror of the base images. Keep the `@sha256:` digests from the Dockerfile; another digest is another image. |

Any other change, such as the platforms or attestations added with
`--provenance` or `--sbom`, gives a different digest that no release attested.

### Extending the image

To add tools, certificates or wrapper scripts, start from the published image
rather than editing this Dockerfile, so the suite stays the one that shipped:

```dockerfile
FROM nvcr.io/<dsx-org-team>/nvke-validate:v0.2.0
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
directory causes suite initialization to fail. The old name
`NKE_CONTROLLER_RELEASES_DIR` is deprecated and still read when the new one is
unset, until [nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399)
removes it.

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
`oidc.managementOIDCAuth.enabled` in `nke-controller-config`), which would make `UpdateCluster{enableOidc}` a no-op — there is no
supported way to create an OIDC-off cluster on such a site. So the brownfield entry
creates its cluster with the CWE create-default flipped **off**
(`nvke.SetOIDCEnabled(false)` — a live edit of `nke-controller-config` and a controller
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

## Site controller lane

`site_controller_test.go` (label `site-controller`) builds its own site controller
on a Forge node and runs the `nico-forge` tenant against it, instead of using the
management cluster in the kube context. `-e2e.site-stage` picks how far it goes;
without it the lane is skipped:

- `provision`: provision the node over the site's jump host and check its egress.
- `bootstrap`: also run `nvkectl bootstrap site` and wait for the API server.
- `full`: also deploy the SBOM stack, wait for CWE, check that NKTP delivers the
  site's telemetry (below), then create and delete a tenant.

Both lanes create the same `nico-forge` tenant and both entries carry that
label, so pick between them with `site-controller`, which only the lane that
builds a site carries. Measured against this tree with `-ginkgo.dry-run` and
`-e2e.site-stage=full`:

| label filter | specs | which |
|---|---|---|
| `nico-forge && !site-controller` | 5 | the tenant, against the site controller in the kube context |
| `site-controller` | 12 | the lane that builds a site, tenant included |
| `nico-forge` | 10 | both |

`-e2e.site-stage`, not the label, is what decides whether a machine is
provisioned: the site lane's tenant entries are built only when it is set, so
a bare `nico-forge` without it selects 5 specs and reaches no hardware.

The node is torn down afterwards (tenants are deleted first). `-e2e.keep-site`, or
`lifecycle.preserveOnFailure` with a failed spec, keeps it.

Prerequisites: a way to reach the jump host (below), the jump host in
`FORGE_JUMP_HOST` (read by `environments/forge.yaml`) and `FORGE_API_KEY`.
Neither the proxy nor the jump host is checked in; secrets are read only from
the environment.

**On DSX** (`-e2e.environment=dsx`) the credential is different and the stage
is limited. Each datahall carries its own SSA client, named for the key it is
filed under in `environments/dsx.yaml`:

```bash
export SSA_ISSUER=…                                  # issuer BASE url; the
export DSX_REDSTONE_DEV1_DH2_SSA_CLIENT_ID=…         # suite appends /token
export DSX_REDSTONE_DEV1_DH2_SSA_CLIENT_SECRET=…     # 15-minute TTL
export DSX_REDSTONE_DEV1_DH2_JUMP_HOST=…             # the rest are identifiers
export DSX_REDSTONE_DEV1_DH2_SITE_ID=… DSX_REDSTONE_DEV1_DH2_TENANT_ID=…
export DSX_REDSTONE_DEV1_DH2_VPC_ID=… DSX_REDSTONE_DEV1_DH2_VPC_PREFIX_ID=…
export DSX_REDSTONE_DEV1_DH2_SSH_KEY_GROUP_ID=… DSX_REDSTONE_DEV1_DH2_INSTANCE_TYPE_ID=…
```

There is no cluster fallback: an unset client fails the run naming both
variables. The names derive from the datahall key, so a credential scoped to
one datahall cannot be picked up by a run against another.

Use `-e2e.site-stage=provision` or `bootstrap`. `full` is refused before a
machine is taken: its tenant half decodes the environment as a Forge
declaration, which a DSX one is not.

redstone-dev1-dh2 is **arm64 only**, and an absent `arch` capability defaults
to amd64, so a hand-run has to ask for arm64 and stage arm64 OS images --- the
command above passes no `-e2e.capabilities` and would fail in `Resolve` with
"no declared site offers a amd64 CPU shape":

```bash
go test -timeout 5h . -args \
  -ginkgo.label-filter=site-controller -ginkgo.v \
  -e2e.config=e2e-config.yaml -e2e.environment=dsx -e2e.site-stage=provision \
  -e2e.capabilities=arch=arm64 \
  -e2e.versions=controller=<…>,os-image=<arm64 url>,os-image-bootloader=<arm64 url>,…
``` Build the inputs, then run; with the `site-inputs` defaults
no `site:` block is needed at all:

```bash
gmake site-inputs SITE_NVKE_CONTROLLER_VERSION=$(git rev-parse --short=12 HEAD)
go test -timeout 5h . -args \
  -ginkgo.label-filter=site-controller -ginkgo.v \
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
once one is cut past #850.

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
`internal/nktp` runs the same checks as `nktp/hack/e2e-validate.sh` does on
Kind, a span it pushes through the node Collector itself, and what only a site
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
