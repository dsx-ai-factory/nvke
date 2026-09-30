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
kubectl --context kind-nke-state -n org-example get nkeclusters,nodepools
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
