# Management Engine Development

From the repository root, activate the [shared toolchain](../docs/development.md#getting-the-tools)
and enter the module:

```sh
flox activate
cd management-engine
```

Run commands below from this directory. Outside a Flox shell, use
`flox activate -- make <target>`. Have `kubectl` installed on your `PATH`;
Flox does not provide it. Without Flox, also install Go, GNU Make, Helm, and
Tilt 0.36+ (required for Helm 4). Make installs the pinned Kind CLI.

## Two-cluster environment

Start Docker. If using Colima, run `colima start` if stopped, then select it:

```sh
unset DOCKER_HOST DOCKER_CONTEXT
docker context use colima
docker info --format 'Docker engine: {{.Name}}'
```

Kind supplies Kubernetes; Colima's built-in k3s is unnecessary. Allow memory for
image builds and all running clusters (start with 8 GiB). For bootstrap timeouts,
check the Docker engine and VM memory (`colima ssh -- free -h`) before retrying.

```sh
make dev
```

This creates or reuses the following single-node Kind clusters, generates the
chart, installs datastore resources, renews credentials, and starts Tilt:

| Cluster | Contents |
| --- | --- |
| `nke-management` | Manager and credential Secret in `management-engine-system` |
| `nke-state` | CRDs/RBAC; identity and election Lease in `management-state-system` |

Press **Space** to open [Tilt](http://localhost:10351/). Keep the command running;
Ctrl+C stops Tilt and its dashboard. If the port is occupied, stop the existing
Tilt session before starting another.

Tilt rebuilds the manager and updates datastore CRDs/RBAC on source changes.
After editing deployment sources under `config/`, run `make chart-check`.
This environment focuses on the manager and datastore. Add the provider
controllers and backing services needed for the lifecycle behavior you are
developing. See [deployment topology](README.md#deployment-topology).

Use `make dev-up` for setup without Tilt. Reuse requires a compatible node layout
and pinned image; incompatible clusters must be explicitly deleted. Checkouts
sharing a Docker engine and name share clusters. For an independent environment:

```sh
make dev DEV_NAME=my-engine TILT_PORT=10352
```

This creates `my-engine-management` and `my-engine-state`. Use the same Docker
engine and `DEV_NAME` for all lifecycle commands. The ignored kubeconfig is
`.local/dev/<DEV_NAME>/kubeconfig`; setup leaves your normal kubeconfig untouched.
For direct commands against the default environment:

```sh
export KUBECONFIG="$PWD/.local/dev/nke/kubeconfig"
kubectl --context kind-nke-state get namespaces
```

## CWE integration

The NKECluster controller creates, observes, and deletes clusters through the
Cloud Workflow Engine (CWE) API. CWE owns provisioning and teardown, including
Temporal workflows and CAPI resources. Updates to existing clusters are not yet
supported.

### Local integration test

Use the shared toolchain, including the Temporal CLI, with Docker running:

```sh
make dev-cwe       # start the combined environment and keep Tilt running
make test-cwe      # run from a second terminal once the services are ready
make dev-cwe-down  # stop Tilt first, then remove the environment
```

This creates the dedicated `nke-cwe-management` and `nke-cwe-state` Kind clusters.
The services cluster runs management-engine, CWE, Temporal, and CAPI; the state
cluster stores NKECluster resources. Open [Tilt](http://localhost:10353/) or the
[Temporal UI](http://localhost:18081/). The manual `cwe-integration-test` resource
under Tilt's **tests** group runs the same test command.

Use `make dev-cwe-up` for setup without Tilt. Pass the same `DEV_NAME` to each
command when choosing another name. The launcher uses CWE's existing Tiltfile
through its local extension hook and requires an unused
`controller/tilt_config.json`; use a separate checkout if you already have one.
Only one combined environment per checkout can own that configuration.

The Ginkgo suite verifies authenticated API requests, persisted remote identity,
Temporal workflow input, successful CAPI apply activity, and the resulting CAPI
resources. Its [binding](test/integration/testdata/bindings.json) uses the Docker
provider and pauses machines before provisioning. No workload nodes or hardware
are provisioned; a local load-balancer container may be created. The workflow
remains pending at this boundary, so a passing test does not mean the workload
cluster is ready. Leave the machine pause in place for this test.

Inspect status in the state cluster and runtime logs in Tilt:

```sh
kubectl --kubeconfig .local/dev/nke-cwe/kubeconfig --context kind-nke-cwe-state \
  -n cwe-integration get nkeclusters -o yaml
```

Resources remain available after testing. Proof JSON and JUnit reports are saved
under `.local/dev/<DEV_NAME>/cwe/results/` and survive teardown. Teardown removes
the dedicated clusters, owned load balancers, and credentials. Rerun Tilt's
`cwe-credentials` resource to renew CWE credentials; `make test-cwe` also renews
them before each run. Datastore credentials use [the usual refresh](#credentials)
with the same `DEV_NAME`.

### Configure an existing CWE service

Provide `--cwe-endpoint` and `--cwe-bindings` together, plus `--cwe-token-file`
for bearer authentication. Without CWE configuration the manager runs without
provisioning. CWE must have control-plane-controller provisioning enabled
(`PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true`).

Bindings are a JSON array selecting `namespace`, `channel`, optional `version`,
and `controlPlaneClass`, with a `template` containing an existing CWE cluster
create request. An omitted version matches only requests without a channel
version. Templates supply the Kubernetes version, control-plane size, and provider
settings; omit `name`, `network`, and `nodePools`, which the controller controls.
The [local binding](test/integration/testdata/bindings.json) and
[matching cluster](test/integration/testdata/cluster.yaml) demonstrate the format;
configure provider resources and profiles for your service.

This integration accepts a single IPv4 pod range and service range with the
`cluster.local` service domain. Worker pools/networking, custom authentication,
public API access, and accelerated-runtime installation are not yet supported.
The general API samples include these future features and cannot be submitted
unchanged to this integration.

For Helm/Tilt, copy the [CWE values example](config/samples/cwe-values.yaml):

```sh
mkdir -p .local/cwe
cp config/samples/cwe-values.yaml .local/cwe/values.yaml
```

Set the service URL and namespace in the copy; retain its datastore and leadership
arguments because Helm replaces argument lists. Supply `.local/cwe/bindings.json`
and `.local/cwe/token`, then mount them in the default development environment:

```sh
make dev-up
export KUBECONFIG="$PWD/.local/dev/nke/kubeconfig"
kubectl --context kind-nke-management -n management-engine-system create configmap management-cwe-bindings \
  --from-file=bindings.json=.local/cwe/bindings.json --dry-run=client -o yaml | \
  kubectl --context kind-nke-management apply -f -
kubectl --context kind-nke-management -n management-engine-system create secret generic management-cwe-token \
  --from-file=token=.local/cwe/token --dry-run=client -o yaml | \
  kubectl --context kind-nke-management apply -f -
DEV_VALUES=.local/cwe/values.yaml make dev
```

Use an endpoint reachable from the manager Pod; its `localhost` is not your host.
HTTPS verifies certificates; HTTP is supported for local/service deployments.
Bindings load at startup, so restart the manager after changing them. Tokens are
reread per request after the mounted Secret refreshes. Keep both files out of
source control. Applying a cluster to a live CWE service requests real provisioning.

Restrict writes to bindings and cluster status to the controller and trusted
administrators. `--namespace` filters watches, not RBAC, and bindings do
not make CWE credentials tenant-scoped. Reserve `nke-<NKECluster UID>` names
for this integration; recovery relies on coordinated API writers.
`status.provisioning` pins the endpoint and creation generation, then the returned
cluster ID. Keep bindings stable while creation is pending: retries rebuild the
request from the spec and bindings. Existing clusters are not updated automatically.

If a create response is lost, reconciliation recovers the cluster by its UID-derived
name. After `CreateRejected`, correct the spec, or correct the bindings and restart
the manager. An earlier attempt with an unknown outcome keeps its checkpoint;
`CreateOutcomeUnknown` retains the finalizer until deletion can be resolved.
For manual recovery, stop the manager and check that name in CWE. If the cluster
exists, restart the manager to recover its ID. If no cluster exists, confirm that
no create request remains in flight before clearing the checkpoint and conditions:

```bash
kubectl --context "$STATE_CONTEXT" -n "$NAMESPACE" patch nkecluster "$CLUSTER" \
  --subresource=status --type=merge -p '{"status":{"provisioning":null,"conditions":null}}'
```

Restart the manager to retry creation, or finish deletion if requested. Do not
remove the finalizer to bypass unresolved provisioning.

## Controller logs

Controllers registered with the manager share the `management-engine` Tilt
resource. Its **NKECluster logs** link filters runtime logs by controller; clear
the filter for all logs.
Tilt enables debug messages with resource names, namespaces, and reconcile IDs.
Use `MANAGER_LOG_LEVEL=info make dev` for quieter output.

## Credentials

```sh
make dev-refresh
```

Renew before the printed expiration. Setup also renews credentials; source edits
do not. Restart the manager if it exited after losing leadership.

## Sample resources

See [sample manifests](config/samples) for examples of the custom resources.
With the environment's `KUBECONFIG` exported:

```sh
kubectl --context kind-nke-state create namespace org-example --dry-run=client -o yaml | \
  kubectl --context kind-nke-state apply -f -
kubectl --context kind-nke-state -n org-example apply -k config/samples
kubectl --context kind-nke-state -n org-example get nkeclusters
# Remove the samples:
kubectl --context kind-nke-state -n org-example delete -k config/samples
```

Use these resources to inspect reconciliation through status and conditions.

## Cleanup

Stop Tilt, then delete both named clusters, their data, and local credentials:

```sh
make dev-down
```

This includes clusters reused from another checkout. To remove only the runtime
and retain datastore objects, keep the environment's `KUBECONFIG` exported and run
`tilt down --context kind-nke-management` (adjust the context and `DEV_NAME` for a
custom environment).

## Build and tests

See the [command summary](README.md#development) for build, generation, drift
checks, and test targets.

Before a PR, run `make preflight MODULES=management-engine` from the repository
root. Follow the [workspace guide](../docs/go-workspace.md) for dependency changes.

### Tests

`make test` runs API tests and controller envtest fixtures; use `test-api` or
`test-controller` separately. Envtest downloads matching API-server binaries.
See [Writing controller tests](docs/writing-tests.md) for fixture conventions and
`make test-update` for regenerating goldens. For installation testing, use a
separate environment without Tilt:

```sh
make docker-build
make dev-up DEV_NAME=nke-test
make test-install DEV_NAME=nke-test
make dev-down DEV_NAME=nke-test
```

This checks deployment, datastore access, reconciliation, upgrades, and state
retention after uninstall. It cleans up its release and org namespace.
Use end-to-end tests for workload lifecycle behavior.

## Add an API or controller

Add API types and markers under `apis/v1`, register them with `SchemeBuilder`,
and add the import path to [PROJECT](PROJECT). Generate manifests and add the new
CRD to `config/crd/kustomization.yaml`.

For an existing API without a controller, replace `Example` with its kind:

```sh
make kubebuilder
bin/kubebuilder create api --group internal --version v1 --kind Example \
  --resource=false --controller=true
```

Implement reconciliation under `controllers/`; scaffolding registers it in
`cmd/main.go`. Declare watches, indexes, and RBAC markers, then regenerate.
Reconcile idempotently from stored desired state and report progress in status.
Use the API reader for uncached reads. Establish ownership before adopting
resources managed by other controllers; separate election Leases do not prevent
competing writes.

Preserve `PROJECT` metadata and scaffold markers. Use `--resource=false` to keep
the existing `apis/` layout; resource scaffolding creates `api/`. Keep single-group
mode to avoid the unimportable `internal/controller/internal` path.

Generation builds `dist/install.yaml` from `config/default`, then generates the
Helm chart. Chart metadata, values, and helpers are maintained separately; do not
use `--force`. Generation does not update `PROJECT`, build an image, or change
image settings in `chart/values.yaml`.
