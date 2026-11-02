# Cloud Workflow Engine

Orchestrate NVKE cluster creation and lifecycle operations.

## Tool Dependencies

The Flox environment is **repository-wide** and lives at the repository root, not
here. One environment serves every module, so the tools this module needs and the
tools `agent/`, `cli/` and `os-image/` need cannot drift apart. It pins `go`,
`make`, `golangci-lint`, `kind`, `clusterctl`, `helm`, `ko`, `tilt`, `temporal`,
`spire-agent`, `actionlint` and `shellcheck` to the versions CI uses.

Install Flox if you do not have it:
```bash
brew install flox colima
```

The supported Flox CLI version is pinned in `.flox-cli-version` at the repository root. If your installed version differs, install the pinned release using the [official Flox installation instructions](https://flox.dev/docs/install-flox/install). The direnv integration refuses to auto-activate a different version so it cannot migrate tracked environment files unexpectedly.

Then activate it. `flox activate` searches upwards, so this works from here or from
any other module; the version check needs the root path because that is where the
pin lives:
```bash
root="$(git rev-parse --show-toplevel)"
test "$(flox --version)" = "$(cat "${root}/.flox-cli-version")" && flox activate
```

You'll want to ensure Flox is activated before running CLI commands related to this repo so you're using the pinned tool versions from this repository instead of whatever happens to be installed globally.

**Note:** The development environment commands (`make local`, `make remote`, etc.) will automatically detect and use the flox environment if available, so you don't need to manually activate it for those commands.

### Optional: Using direnv for automatic environment setup

If you want to automatically activate the Flox environment and load personal environment variables when entering this directory, you can use [direnv](https://direnv.net/):

```bash
brew install direnv
```

Then follow the [direnv setup instructions](https://direnv.net/docs/hook.html) to hook it into your shell.

Once installed, run `direnv allow` in this directory. Now whenever you `cd` into this directory, direnv will automatically:
- Activate the Flox environment (no need to run `flox activate`)
- Load any personal environment variables from `.env.local` (if it exists)

To set up personal environment variables:
```bash
cp .env.local.example .env.local
# edit .env.local for host-local workflows like `make run-local` and `make run-remote`
```

Then edit `.env.local` to add your personal settings for host-local CWE runs. This file is gitignored and won't be checked into the repository.

### Go module configuration

Nothing to set. Every dependency of this module is public: the
`gitlab-master.nvidia.com` ones went in #76, and
`github.com/dsx-ai-factory/cluster-api-provider-nico`, which was internal after that,
is public now and served by `proxy.golang.org`. `GOPRIVATE` is no longer needed
for any of it.

`make build` from the repository root compiles every module with no internal
credentials. A cold build downloads public dependencies into the Go cache.

### API module (for consumers)

The `api/` directory is a separate Go module that other projects (e.g. controllers, infra) can depend on without pulling in the full CWE. It is versioned with semantic version tags `api/vX.Y.Z`, published automatically when `api/` changes on the default branch.

To depend on a specific version:

```bash
go get github.com/dsx-ai-factory/nvke/controller/api@v0.0.1
```

Use the latest tagged version or a specific semver (e.g. `@v0.0.5`) as needed.

**If you work in this repo (CWE):** You do not need to run `go get` when the API version changes. The root module uses a `replace` directive in `go.mod` that points to the local `./api` directory, so builds and tests always use the API code in this repository. The tagged versions are for external consumers (e.g. controllers, infra) that import the API as a dependency.

## Choose a Development Path

- `CAPI + Tilt (recommended)`: bring up a local Kind management cluster with CAPD, then run `tilt up` for an end-to-end local stack.
- `Manual local/remote worker run`: work on the CWE worker without the local CAPI management-cluster flow by using `make run-local` or `make run-remote`. `make run-local` installs the release CRDs and the fixture releases into the cluster in `KUBECONFIG` first (`make releases-install`); `make run-remote` installs nothing, so the cluster it reads must already have the releases (a fleet cluster gets them from Argo CD). The process still exits when the Cluster API `Machine` CRD is missing: `make releases-install` does not install it, and `make capd-up` does.
- `UI-only development`: port-forward a live CWE deployment and run the local UI dev server against it.

For the controller-to-node-agent TLS/JWT transport configuration, see
[Configuring node-agent TLS/JWT authentication](docs/configuring-node-agent-tls-jwt.md).

External node-agent mTLS identity is disabled by default. When enabled, the
controller mounts the configured externally managed Secret read-only and
presents that certificate instead of its SPIRE X509-SVID. This setting is
independent of the application-level authentication protocol used with the node
agent. In this mode, the agent's self-signed server certificate is not verified.
For NICo nodes, the controller exposes generated files through the OS-profile
template value `.Files`. Each entry contains its path, owner, permissions,
encoding, and content in the representation declared by that encoding.
Compatible profiles append those entries to their existing cloud-init
`write_files` list and quote the metadata and content. CAPI serializes the same
file model directly. In every mode, both paths replace `/etc/default/nvke-agent`
with a plain-text configuration containing the node-agent port. External mTLS
also writes the CA with `encoding: b64` and points the agent at it with
`--nvke-controller-client-ca-bundle`, so it requires node agent 0.3.0 or later.
The CA lands at `/etc/nvke/controller-ca.crt` on a node whose agent reports
0.3.0 or later, and at `/etc/nke/controller-ca.crt` otherwise, including a new
node whose agent has not reported a version yet. These
controller files follow any provider or profile files so the
defaults win within `write_files`; NICo profiles and CAPI users may replace the
agent configuration later through `runcmd` or `preKubeadmCommands`. The
controller does not validate profile compatibility.
Release the compatible agent, DSX profiles, and images before enabling external mTLS.
Secret updates take effect in the controller after its reloader-triggered pod
restart, but do not rotate the CA on existing nodes.

## CAPI + Tilt Development (Recommended)

This is the recommended local workflow for developing against a Kind management cluster with CAPD.
Tilt improves the Kubernetes inner dev loop: it rebuilds and redeploys services when code or manifests change, keeps a multi-service local stack coordinated in one place, and gives you a single UI for logs, status, port-forwards, and helper actions.

Full nico-vms (real libvirt VMs / amd64 releases) usually needs an x86_64 Linux host with KVM.

### First Run

1. Prepare the Docker host. On macOS this starts Colima. On Linux it loads the
   vTPM proxy module on the host kernel:

```bash
make host-up
```

On macOS, point Docker at the Colima socket. This can live in your shell rc or
`.env.local`:

```bash
export DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
```

`make host-up` is `make colima-up` on macOS. That prepares the Colima VM,
including `/dev/vtpmx` for the TPM-backed Forge flow. On Linux it runs
`sudo ./hack/ensure-vtpm-proxy.sh` against the host kernel. A fresh Colima VM
may take longer the first time while the extra kernel module package is
installed.

**macOS: reach the Kind Docker subnet directly**

CAPD tenant clusters publish apiserver URLs on the Kind Docker bridge (for example
`https://172.18.0.4:6443`). Those IPs are not reachable from the Mac host by default
because Docker/Kind run inside the Colima VM. Install
[`docker-mac-net-connect`](https://github.com/chipmk/docker-mac-net-connect) to get
direct L3 access to container IPs, including Colima:

```bash
brew install chipmk/tap/docker-mac-net-connect
sudo brew services start chipmk/tap/docker-mac-net-connect
```

Use v0.1.6 or newer (`brew info chipmk/tap/docker-mac-net-connect`) — earlier
releases predate both Colima support and the fix for Docker Engine 28+'s stricter
bridge-subnet filtering. Once the service is running it auto-detects the active
Docker context, adds/removes routes as Docker networks come and go, and survives
Mac reboots and Colima restarts without any manual `route` commands.

2. Create the local Kind management cluster and install CAPI + CAPD:

```bash
make capd-up
```

3. Start Tilt:

```bash
# optional if your kind cluster name is not controller-capi
# export KIND_CLUSTER_NAME=<your-kind-cluster-name>

# required: copy the template and fill in the credentials it declares
cp .env.tilt.example .env.tilt

tilt up
```

- Tilt only runs against the expected Kind context (`kind-${KIND_CLUSTER_NAME}` or the current `kind-*` context if `KIND_CLUSTER_NAME` is unset).
- Plain `tilt up` keeps the default CAPD-oriented provider set and sets `controllers.nico.enabled=false`. Listing `nico` or `nico-vm` in `tilt_config.json` sets it to `true`; those providers install the `NicoMachine` CRD.
- If you want to enable NICo for the local dev flow, set `providers` in
  `tilt_config.json` to include `"capi_core"`, `"bgp-containers"`, and
  `"nico"`. That one provider starts nico-sim and installs one CAPNICo
  controller through Helm. VM and container clusters select different sites
  through the same endpoint and controller. `cp tilt_config.json.example
  tilt_config.json` provides the local starting point.
- `nico_sim_backends` controls which local nico-sim backends Tilt prepares.
  `["container"]` uses container-backed mock NICo and skips VM-only host
  requirements such as dnsmasq, Linux inotify sysctl tuning, BGP route setup,
  and VM log shipping. This is the default; use `["container", "vm"]` when you
  explicitly want the local nico-vms path.
- CAPNICo reaches nico-sim through `NICO_SIM_ENDPOINT_HOST` when set. Otherwise
  Tilt uses `host.docker.internal` on macOS and the Kind gateway on Linux.
- `.env.tilt` holds Tilt's inputs: the credentials, toggles such as `NKE_AUTH_ENABLED` and `TENANT_ACCESS_ENABLED`, and the few variables the Tiltfile passes to the controller (`LOG_LEVEL`, `SPIRE_SERVER_API_ADDRESS`, ...). The controller's other settings come from the chart values, rendered into `nvke-controller-config`. Keep host-local settings in `.env.local`.
- `DGXC_TENANT_USER`, `DGXC_TENANT_PASSWORD`, `CNPG_SUPERUSER_PASSWORD`, `CNPG_CLOUD_PASSWORD`, and `CNPG_TEMPORAL_PASSWORD` have no defaults — set them in `.env.tilt` or export them, or `tilt up` fails with the list of what is missing. `CNPG_SUPERUSER_PASSWORD` is what the local postgres chart is installed with, so changing it after the first `tilt up` requires deleting the postgres PVC.
- Personal Tilt customizations can go in `tilt.d/*.tilt` or `tilt.d/*.star`.
- CWE defaults to `SPIRE_SERVER_ADDRESS=host.docker.internal`, and Tilt's chart values set `spiffe.spire.serverPort` to `30440`. On Linux Docker, if `host.docker.internal` is not available, override `SPIRE_SERVER_ADDRESS` in `.env.tilt` to your host's gateway IP (for example: `ip -4 route show default | awk '{print $3}'`).

### CAPNICo simulation: nico-vms and nico-containers

Bootstrap the management cluster with CAPI core:

```bash
make capi-core-up
```

Then enable the single NICo provider:

```json
{
  "providers": ["capi_core", "bgp-containers", "nico"],
  "nico_sim_backends": ["container"]
}
```

With no additional NICo configuration, Tilt starts nico-sim, installs the
released CAPNICo Helm chart, and points the `nico-credentials` Secret at
nico-sim. The example config prepares only the seeded `nico-containers` site.
To operate the seeded `nico-vms` site as well, set
`"nico_sim_backends": ["container", "vm"]`. Export `NGC_API_KEY` in the shell
that runs Tilt so Helm and the management cluster can pull the released chart
and image.

Use the buttons on the `nico-sim` resource in the Tilt UI to create the VM,
inspect its status, read its console log, and delete it. The create and delete
buttons run scripts that can also be called directly:

```bash
./hack/capnico-create-instance.sh
./hack/capnico-delete-instance.sh
```

The VM login defaults to nico-sim's diagnostics credentials
(`nico-diag`/`nico-diag`). Set `VM_USER` and `VM_PASSWORD` to override them.

#### Building CAPNICo from a local checkout

To test CAPNICo changes without the released chart/image, point the same
provider at a source checkout:

```json
{
  "providers": ["capi_core", "bgp-containers", "nico"],
  "capnico_source_dir": "../../cluster-api-provider-nico"
}
```

Relative paths resolve against `controller/`; `CAPNICO_SOURCE_DIR` overrides the
config for a one-off run. Source mode builds `cluster-api-provider-nico:local`,
loads it into Kind, stages the checkout's chart with the local reboot-annotation
overlay, and still installs CAPNICo through Helm. It does not use `NGC_API_KEY`
or an image-pull Secret. Docker BuildKit is required, and Tilt watches the
checkout so source changes retrigger the provider installation.

Leave `capnico_source_dir` unset to return to the released chart flow.

#### External NICo endpoint

To use an external NICo-compatible endpoint instead of nico-sim, configure it
under `nico`. In that mode the provider skips nico-sim and its local node image:

```json
{
  "providers": ["capi_core", "nico"],
  "nico": {
    "endpoint": "https://nico.example.com",
    "org_id": "example-org",
    "api_name": "forge",
    "environment": "test"
  }
}
```

Set the external credential with `NICO_TOKEN`; `NICO_ENDPOINT`, `NICO_ORG_ID`,
and `NICO_API_NAME` can override the corresponding config fields for one-off
runs. Helm is always required; source mode additionally needs Docker, Kind, and
BuildKit.

#### LaunchLayer

CAPLL has no published chart or image yet, so the `launchlayer` provider always
builds it from a local `cluster-api-provider-launchlayer` checkout. It requires
`capi_core`, and with SPIFFE on it needs the `dsx-imds-attestation` provider
too (see below):

```json
{
  "providers": ["capi_core", "launchlayer", "dsx-imds-attestation"],
  "capll_source_dir": "../../cluster-api-provider-launchlayer",
  "dsx_imds_attestation_source_dir": "../../dsx-identity"
}
```

Relative paths resolve against `controller/`; `CAPLL_SOURCE_DIR` overrides the
config for a one-off run, and Tilt fails at startup when neither is set. The
provider builds the CAPLL manager and CAPLL's fake LaunchLayer API from the
checkout, loads both into Kind, deploys the fake into `fake-ll-api`, and
installs the checkout's chart into `capll-system` with a credentials Secret
pointing at the fake. Helm and Docker are required, and Tilt watches the
checkout so source changes retrigger the installation. Enabling the provider
also adds `launchLayer` to the controller's `supportedInfraProviders`.

Tilt runs the controller with SPIFFE enabled, and a SPIFFE-enabled LaunchLayer
node attests to SPIRE only through DSX IMDS; with the chart's default `tpm`,
the controller rejects LaunchLayer requests with 400. The
`dsx-imds-attestation` provider sets everything DSX IMDS needs: the
controller's attestation method, `nodeAgent.dsxImds.operatorTenant` and
`nodeAgent.dsxImds.endpoint`, and the SPIRE server's `dsx_imds` node attestor.
Setting only the attestation method is not enough: the chart refuses to render
without `operatorTenant`. See
[hack/dsx-imds-attestation/README.md](hack/dsx-imds-attestation/README.md).

### TPM support in Docker

TPM is one of the node-attestation mechanisms CWE/NVKE uses to establish SPIRE
node identity for SPIFFE-enabled nodes. In the TPM mode, Forge exposes the
node's EK certificate through NICo, CAPNICo publishes the TPM EK pubhash on the
`NicoMachine`, and CWE annotates the owning CAPI `Machine` with the TPM-shaped
SPIRE parent ID. The Machine SPIRE entry controller then reconciles the related
cluster entry.

There are three attestation modes in the local cluster-create paths, set with `nodeAgent.spireAttestationMethod`:
- `joinToken`
- `tpm`
- `dsxImds` (see the `dsx-imds-attestation` provider above)

The CAPI cluster creation path supports all three modes; `dsxImds` needs the DSX IMDS attestor plugin in the node image. The Docker-based Forge flow
keeps the same TPM-shaped contracts as a real Forge node and swaps only the
physical TPM for a software-backed vTPM chain:

1. `make host-up` prepares the kernel that runs Docker to expose `/dev/vtpmx`
   via the `tpm_vtpm_proxy` module. On macOS that kernel is the Colima VM. On
   Linux it is the host. That is the bridge `swtpm` needs in order to ask Linux
   for a real TPM device.
2. Each local node container starts `swtpm --vtpm-proxy` through
   `swtpm-local.service` in `hack/capd-kindest-node/Dockerfile`. That causes
   the kernel from the previous step to allocate a vTPM-backed TPM device for that node,
   which the image then mirrors into the container as `/dev/tpm0` (and
   `/dev/tpmrm0` when available).
3. `spire-agent` still uses the TPM attestor plugin against that Linux
   TPM device, so the node-side SPIRE flow stays aligned with the normal
   device-oriented attestation path.
4. During the same startup sequence, `swtpm_setup` writes an EK certificate
   under `/var/lib/swtpm`, and the node image normalizes it to
   `/var/lib/swtpm/ek.cert`. The nico-containers backend in nico-sim reads that
   file and returns it as `tpmEkCertificate`, which lets CAPNICo publish the
   normal `NicoMachine.status.tpmEkPubHash` value for the CAPI controllers.

This is still lower fidelity than real hardware: the TPM is software-backed,
the nodes are Docker containers, and nico-sim emulates the NICo API. The
benefit is that CWE, SPIRE, and the local CAPNICo flow can keep exercising the
same TPM-oriented CAPI contracts without introducing local-only branches in the
production workflow/controller code.

### Daily Use

```bash
tilt up
```

- Once Tilt is running:
  - UI: http://localhost:8080
  - Swagger: http://localhost:8080/api/docs
  - Temporal UI: http://localhost:8081
- CWE pod logs are available in the native `nvke-controller` Tilt resource.
- Use the Tilt buttons for the common local actions below.

### Common Actions

- Create a sample CAPD cluster from the `nvke-controller` Tilt resource.
- Create a sample local Forge cluster from the `nvke-controller` Tilt resource when NICo is enabled.
- Create a sample local NICo cluster from the `nvke-controller` Tilt resource when NICo is enabled.
- Restart CWE from the `nvke-controller` Tilt resource.
- Show SPIRE entries or list connected SPIRE agents from the `spire-server` Tilt resource.

CLI equivalents if you prefer:

```bash
# Create a sample CAPD cluster
./hack/create-capd-cluster.sh

# Upgrade a CAPD cluster (rebuilds cwe-capd-kindest-node:local for the target
# kindest/node tag, then PATCHes controlPlaneKubernetesVersion + maxSurge)
TARGET_VERSION=1.35.0 ./hack/capd-upgrade.sh
# or: TARGET_VERSION=1.35.0 make capd-upgrade

# Create a sample local Forge cluster when NICo is enabled
./hack/create-forge-cluster.sh

# Create a sample local NICo cluster when NICo is enabled
./hack/create-local-nico-cluster.sh

# Create additional CAPD clusters by changing the cluster name or desired node count
CLUSTER_NAME="my-cluster" CONTROL_PLANE_DESIRED_COUNT=3 WORKER_DESIRED_COUNT=3 ./hack/create-capd-cluster.sh

# Show SPIRE server entries
kubectl exec -n spire-server spire-server-0 -c spire-server -- bin/spire-server entry show

# List connected SPIRE agents
kubectl exec -n spire-server spire-server-0 -c spire-server -- bin/spire-server agent list
```

### Forge-backed NICo Flow

This is a separate workflow from the normal daily Tilt loop. Use it when you
want to exercise CAPNico against real Forge/NICo instead of the local
container-backed nico-sim flow that Tilt sets up by default.

The helper script lives at `./hack/create-forge-nico-cluster.sh`. It assumes:

- CWE is reachable through a port-forward to the in-cluster service
- a provider-level NICo credentials Secret exists in the `cluster-api-provider-nico`
  manager namespace (`capnico-system` by default)

#### Provider-level credentials Secret

`cluster-api-provider-nico` reads NICo credentials from a Secret in its own
manager namespace; one Secret is shared by every `NicoCluster` the provider
manages. See the [cluster-api-provider-nico
README](https://github.com/dsx-ai-factory/cluster-api-provider-nico#install)
for the full set of keys.

Pull the Forge tenant token from Vault and apply it as a Secret in the
`capnico-system` namespace:

```bash
vault login -method=oidc -path=oidc role=ngc
NICO_TOKEN="$(vault kv get -field=servicekey secrets/groups/nkek8s/ngc/forge-tenant-nke/forge-access-info)"

kubectl create secret generic nico-credentials \
  --namespace capnico-system \
  --from-literal=endpoint=https://api.ngc.nvidia.com \
  --from-literal=orgID=novxwn552ssc \
  --from-literal=token="${NICO_TOKEN}" \
  --from-literal=apiName=forge
```

`cluster-api-provider-nico` re-reads the Secret on every reconcile and rebuilds
the NICo client whenever the Secret's `resourceVersion` changes, so any
external rotation is picked up without restarting the manager.

#### Creating a cluster

```bash
# Port forward the CWE API
tsh login
kubectl -n cloud-workflow-engine port-forward svc/cloud-workflow-engine 18080:8080

# Create the Forge NICo cluster
NVKE_CONTROLLER_HTTP_PORT=18080 ./hack/create-forge-nico-cluster.sh
```

### Cleanup

Delete the workload cluster before deleting the Kind management cluster so CAPD can clean up the Docker machines it created.

`tilt down` is not the full teardown for this workflow. Tilt manages some resources natively, but this setup still relies on Helm installs, applied manifests, and other local actions that remain in the cluster after the Tilt session exits.

```bash
# Prefer deleting through CWE so its state is cleaned up too
curl -X DELETE http://localhost:8080/api/clusters/<cluster-id>

# Or delete all local workload clusters directly
kubectl delete cluster --all --all-namespaces --wait=false

# Then tear down the Kind management cluster
make kind-down
```

### Nuclear Clean up
Deleting the colima VM gets you back to a clean state.
```bash
make colima-clean
```

## Manual Local/Remote Worker Run

**Quick Start**: See `Choose a Development Path` above if you want the recommended local CAPD/Tilt flow or the shorter `make run-local` / `make run-remote` path.

The following are the individual manual steps if you prefer to run things yourself:

Typically when running locally, use a local Temporal server for CWE workflows or connect to a remote Temporal deployment.

Start a local temporal server for the CWE workflows (use `flox activate` to get the `temporal` CLI):
```
temporal server start-dev
```

And you'll need a local Postgres for CWE data:
```
docker run -d --name=cwe-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:17.4
```
(If you ever want to start fresh with an empty database, remove the container with `docker rm -f cwe-postgres` and then start a new one)

To use remote Temporal, set up your kubeconfig and SSH tunnel for the management cluster, then run a port forward to the Temporal service (using non-default ports to avoid conflicts with local Temporal):
```bash
kubectl port-forward -n temporal svc/temporal-frontend 17233:7233
```
You'll probably also want the Temporal UI (open in browser at http://localhost:18233):
```bash
kubectl port-forward -n temporal svc/temporal-web 18233:8080
```

CWE resolves every OS image from `MachineImage`, `ClusterRelease` and
`NodePoolRelease` resources, and exits at startup if their CRDs are not
installed in the cluster in `KUBECONFIG`. The controller chart installs them, so
check what the cluster already has before installing anything:
```bash
for resource in clusterreleases nodepoolreleases machineimages; do
  kubectl get "${resource}.internal.nvke.nvidia.com"
done
```
On a local development cluster, install the CRDs and this repository's fixture
releases:
```bash
make releases-install
```
Those releases are test fixtures and are cluster-scoped, so do not run that
against a shared site.

And now you're ready to build and run CWE, against the forwarded Temporal port:
```bash
mkdir -p .local
yq '.temporal.hostPort = "localhost:17233"' hack/local-controller-config.yaml > .local/controller-config.remote.yaml
CONTROLLER_CONFIG_PATH=.local/controller-config.remote.yaml make run
```

You should now see the Swagger UI at http://localhost:8080/api/docs.

## API Authentication

NVKE can require a signed JWT on every `/api` request. It is off by default;
mounting a configuration file turns it on.

With it off, any caller that can reach `/api` has full administrative access:
the whole cluster lifecycle, plus the pass-through endpoints that call the
infrastructure provider using NVKE's own credentials.

A token is accepted when all four of the following hold: it was signed by an
issuer you configured, it names NVKE in `aud`, it has not expired, and its subject
matches an admission rule.

Rules come in two shapes: Kubernetes service accounts (namespace plus name) and
SPIFFE IDs (trust domain plus path prefix). In practice you present either a bound
service account token or a SPIRE JWT-SVID.

Verifying the token is not enough on its own. Any workload author who can write a
pod spec can have the kubelet project a token for their own service account,
naming any audience they choose. No RBAC check applies to that audience. Admission
rules keep every service account on the cluster from being an NVKE administrator.

NVKE does no authorization: every admitted caller can do everything. That is a
platform responsibility.

### Local Development

You don't need identity infrastructure to work on this. The cluster's own API
server is an issuer, and the service account tokens it signs are JWTs, so NVKE
verifies them like any other issuer. The e2e suite takes the same path.

For a step-by-step walkthrough of getting a token and calling the API, including
what to check when you get an unexpected 401, see
[Authenticating to the NVKE API](docs/authenticating-to-the-api.md).

`tilt up` deploys an enforcing NVKE with the chart values in
`hack/tilt-controller-api-auth-values.yaml`, the two client identities
`deploy/k8s/nvke-auth-dev.yaml` creates (one admitted, one not), and the binding
described under RBAC Prerequisite.

`nvkectl` uses that admitted identity. After `nvkectl config set-site` with the
kind kubeconfig, CWE commands mint tokens on their own. There is no token
environment variable:

```bash
nvkectl config set-site --name local \
  --url http://localhost:8080 \
  --kubeconfig ~/.kube/config
nvkectl get clusters
```

```bash
./hack/nvke-auth-demo.sh    # four credentials, prints the status code for each
```

Mint a token for `curl` yourself:

```bash
kubectl create token nke-client -n nke-clients \
  --audience=https://nke.tilt.local --duration=10m
```

Permissive mode is a different deployment, not a runtime switch:

```bash
NKE_PERMISSIVE_MODE=true tilt up
```

### Enabling Authentication

For a step-by-step guide to configuring authentication on a deployment you
operate: writing the config, per-issuer recipes, and moving a running site from
permissive to enforcing. See
[Configuring NVKE API Authentication](docs/configuring-api-authentication.md).

With the Helm chart, set the `apiAuth` values:

```yaml
apiAuth:
  enabled: true
  permissive: false
  configMap:
    create: false               # true to render apiAuth.config into both
                                # nvke-controller-config and nvke-auth-config
    name: nvke-auth-config      # mounted either way
  clusterIssuerDiscovery: true  # see RBAC Prerequisite below
```

When the chart renders the configuration (`configMap.create: true`), it puts it
in the `apiAuth` section of `nvke-controller-config`, with `enabled: true`, and
sets no `AUTH_CONFIG_PATH`. For a ConfigMap managed elsewhere it sets
`AUTH_CONFIG_PATH` to the mounted file instead. The chart renders
`apiAuth.permissive` into the `apiAuth` section of `nvke-controller-config`, and
it applies either way. Without the chart, set the `apiAuth` section of
`nvke-controller-config` or `AUTH_CONFIG_PATH` (not both), and
`apiAuth.permissive`, directly. `permissive` may be `true` while `enabled` is
`false`, for a file at `AUTH_CONFIG_PATH`. A top-level `permissiveMode`, where
the setting used to live, is refused at startup with a message naming
`apiAuth.permissive`. The `apiAuth` section must be present in
`nvke-controller-config` whichever you use: `enabled: false` with both lists
empty runs without authentication, and a file with no section does not start.
All of it is read at startup, so changing any of it needs a pod restart. The
chart arranges that restart itself; see
[Authentication Configuration](#authentication-configuration) below.

| State | Configuration | `apiAuth.permissive` | Result |
| --- | --- | --- | --- |
| **Disabled** | `apiAuth.enabled` false and `AUTH_CONFIG_PATH` unset | — | Middleware is not in the chain. Every request served. The default. |
| **Permissive** | `apiAuth.enabled` true, or `AUTH_CONFIG_PATH` set | `true` | Every request evaluated, logged and counted, then served anyway. |
| **Enforcing** | `apiAuth.enabled` true, or `AUTH_CONFIG_PATH` set | `false` | Failures answered 401 or 403. |

`apiAuth.enabled: true` together with `AUTH_CONFIG_PATH` is a startup failure,
and so is an `apiAuth` section with audiences or issuers while `enabled` is
`false`.

A deployment that never sets `apiAuth.enabled` in its values stays disabled
across upgrades.

Permissive is a migration tool, not a setting to leave on. Use it to see who would
be rejected before anyone actually is: watch
`nvke_auth_requests_total{outcome="would_reject"}` until it stays at zero, then
turn it off. The `reason` and `issuer` labels say what was wrong and whose token
it was. It suppresses the 403 as well as the 401, so no caller is rejected.

If the configuration is set but unreadable or invalid, from either source, NVKE
will not start, rather than starting with authentication disabled.

### Authentication Configuration

Populate the configuration one of two ways: let the chart render it from
`apiAuth.config` (`configMap.create: true`), in which case NVKE reads it from
`nvke-controller-config` and the chart also renders the same values into
`nvke-auth-config`, or supply your own ConfigMap named by
`apiAuth.configMap.name` and leave `create` false, in which case NVKE reads it
from the file mounted at `AUTH_CONFIG_PATH`. Either way the chart mounts
`apiAuth.configMap.name` at `/etc/nke/auth`. `nvkectl` reads the audience from
`apiAuth` in `nvke-controller-config` when that section is enabled, and from
`nvke-auth-config` otherwise. A private CA bundle is simplest inline, as
`jwks.caBundle`. Files a `jwks.caBundlePath` names land in `/etc/nke/auth` too:
through `apiAuth.configMap.extraFiles` when the chart renders the ConfigMap, or
as keys you add to your own ConfigMap, since the chart refuses `extraFiles`
without `create`. Setting `apiAuth.config` without `create`, or `create` without
`apiAuth.config`, fails the render rather than quietly ignoring one of them.

**The chart renders it** when `apiAuth.configMap.create` is true. Put the
document under `apiAuth.config`, carrying `audiences` and `issuers` only --
`apiVersion` and `kind` are supplied by the chart, which ships in lockstep with
the build that accepts them:

```yaml
apiAuth:
  enabled: true
  configMap:
    create: true
    name: nvke-auth-config
  config:
    # No apiVersion or kind: the chart supplies both, and repeating them here
    # renders duplicate keys that authn.Load refuses. The render fails if you do.
    audiences: ["https://nke.example.internal"]
    issuers:
      - name: local-kubernetes
        issuer: "https://kubernetes.default.svc.cluster.local"
        jwks:
          uri: "https://kubernetes.default.svc/openid/v1/jwks"
          caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
          bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
        algorithms: [RS256, ES256]
        admit:
          serviceAccounts:
            - namespace: cloud-workflow-engine
              name: forge-console
```

So the document below is the shape of the *file*, and `apiAuth.config` takes it
from its third line on.

This is the option for a GitOps deployment, where the configuration belongs in
the same values file as everything else and there is nowhere to put a loose
manifest.

The chart checks that `audiences` and `issuers` are present and refuses
`apiVersion`, `kind`, `enabled` and `permissive` inside `apiAuth.config`. Everything else is
checked by the controller at startup, with `auth/authn`'s rules, which are
strict and name the field at fault. A bad value becomes a `CrashLoopBackOff`
rather than a failed render, so read the pod logs, not the Helm output.

An issuer whose `jwks.uri` sits behind a private CA needs its bundle. Put the
PEM inline as `jwks.caBundle`, which needs no file:

```yaml
apiAuth:
  configMap:
    create: true
  config:
    issuers:
      - name: spire
        jwks:
          uri: "https://spire.example.internal:30443/keys"
          caBundle: |
            -----BEGIN CERTIFICATE-----
            ...
            -----END CERTIFICATE-----
```

`caBundle` and `caBundlePath` are alternatives; set at most one, and a
`caBundle` with no PEM certificate in it is refused at startup. To keep the
bundle as a file instead, `extraFiles` puts it in the same directory. The whole
ConfigMap is mounted, so every key in it is a file at `/etc/nke/auth`:

```yaml
apiAuth:
  configMap:
    create: true
    extraFiles:
      spire-ca.crt: |
        -----BEGIN CERTIFICATE-----
        ...
        -----END CERTIFICATE-----
  config:
    issuers:
      - name: spire
        jwks:
          uri: "https://spire.example.internal:30443/keys"
          caBundlePath: /etc/nke/auth/spire-ca.crt
```

A CA bundle is a public certificate, not a secret. A `caBundlePath` under
`/etc/nke/auth` that names a file nothing supplies fails the render, rather than
leaving the pod to crash-loop on a missing file; a path anywhere else is taken
to be a volume you mount yourself and is left alone.

**Or create it yourself** and leave `create` false, which is the default. The key
must be `auth.yaml`:

```bash
# In the release namespace. The chart mounts the ConfigMap at /etc/nke/auth and
# points AUTH_CONFIG_PATH at auth.yaml within it.
kubectl create configmap nvke-auth-config -n "${RELEASE_NAMESPACE}" \
  --from-file=auth.yaml=./auth.yaml
```

Either way, editing the configuration only takes effect when the pods restart:
it is read once at startup and Kubernetes does not restart anything when a
mounted ConfigMap changes. So whenever `apiAuth.enabled` is true the chart
annotates the Deployment for [Reloader](https://github.com/stakater/Reloader),
which watches both `apiAuth.configMap.name` and `nvke-controller-config` and
rolls the Deployment when either changes, from any source, `kubectl edit`
included. Set
`deploymentAnnotations."configmap.reloader.stakater.com/reload"` to take that
over; the chart leaves an annotation you set alone. Without Reloader running,
restart the pods yourself.

With `create: true`, though, the copy the controller reads is `apiAuth` in
`nvke-controller-config`: change the chart values, or edit that ConfigMap.
`nvke-auth-config` is a copy for `nvkectl` releases that read only it, and for
the files beside it, so editing it alone restarts the pods but changes nothing
the controller enforces; current `nvkectl` reads `apiAuth` in
`nvke-controller-config` first and does not see the edit either.

`hack/tilt-controller-api-auth-values.yaml` is a working example, as chart
values; the full schema and every validation rule live in
`auth/authn/config.go`. The shape:

```yaml
apiVersion: auth.nke.nvidia.com/v1alpha1
kind: AuthenticationConfiguration

# Accepted "aud" claims. Any-of, so list two to rotate without downtime. No entry
# may equal an issuer below: kubeadm defaults --api-audiences to the service
# account issuer, so that would accept every pod's automounted token.
audiences: ["https://nke.example.internal"]

issuers:
  - name: local-kubernetes
    # The exact "iss" claim. Read it from the cluster's
    # /.well-known/openid-configuration; it is not the API server's address.
    issuer: "https://kubernetes.default.svc.cluster.local"
    jwks:
      uri: "https://kubernetes.default.svc/openid/v1/jwks"    # https only
      caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
      bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
    algorithms: [RS256, ES256]     # asymmetric only; "none" and HMAC are refused
    # One of serviceAccounts, spiffe, or groups.
    admit:
      serviceAccounts:
        # Omitting `name` admits every service account in the namespace, present
        # and future.
        - namespace: nke-clients
          name: nke-client

  - name: spire
    issuer: "https://nke.site-controller.internal"     # SPIRE's jwt_issuer
    # A per-issuer list replaces the deployment-wide one rather than adding to
    # it. Omit to inherit; `audiences: []` is a startup error.
    # audiences: ["https://nke-spire.example.internal"]
    jwks:
      uri: "https://nke.site-controller.internal:30443/keys"
      # Or the PEM inline as caBundle; at most one of the two.
      caBundlePath: /etc/nke/spire-ca/ca.crt
    algorithms: [RS256]
    admit:
      spiffe:
        trustDomain: example.org     # bare host: no spiffe:// scheme, no path
        pathPrefixes: [/nke-e2e/]    # segment-wise; /nke-e2e/ never matches /nke-e2e-evil/

  - name: keycloak
    issuer: "https://keycloak.example.internal/realms/nke"
    jwks:
      uri: "https://keycloak.example.internal/realms/nke/protocol/openid-connect/certs"
    algorithms: [RS256]
    admit:
      groups:
        # Any-of against the token's "groups" claim (a JSON string array).
        - nke-operators
```

### RBAC Prerequisite

An issuer pointing at the cluster's own `/openid/v1/jwks` needs NVKE's
ServiceAccount to hold `system:service-account-issuer-discovery`. The endpoint
requires authentication; an anonymous fetch is refused.

Kubernetes already grants that role to every service account, so a stock cluster
needs nothing here. The chart still creates its own binding when
`apiAuth.clusterIssuerDiscovery` is true, which covers clusters that removed or
narrowed the default.

To create the binding by hand:

```bash
kubectl create clusterrolebinding nke-service-account-issuer-discovery \
  --clusterrole=system:service-account-issuer-discovery \
  --serviceaccount=cloud-workflow-engine:cloud-workflow-engine
```

### Calling the API from Scripts and CI

Interactive use is `nvkectl`: it mints the same identity from the site
kubeconfig. `hack/nvke-token.sh` is for `curl` and scripts. It prints a bearer
token, or nothing when authentication is off, so the same call works either
way:

```bash
TOKEN="$(hack/nvke-token.sh)"
curl ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} "${API_URL}/clusters"
```

It reads the audience from the running Deployment and mints an audience-scoped
token through the TokenRequest API, so the caller needs `create` on
`serviceaccounts/token` for the service account being minted for.
`deploy/k8s/nvke-auth-dev.yaml` grants that locally.

Useful overrides:

| | |
| --- | --- |
| `NKE_AUTH_TOKEN` | Use this token and skip discovery. For CI that already holds one. |
| `NKE_AUDIENCE` | Mint for this audience instead of reading it from the Deployment. |
| `NKE_TOKEN_SERVICE_ACCOUNT`, `NKE_TOKEN_NAMESPACE` | Mint for a different identity. |
| `NKE_TOKEN_TTL` | Requested lifetime. Defaults to 30m. |

The bootstrap scripts under `hack/` call it themselves, so `create-capd-cluster.sh`
and friends keep working whether authentication is on or off.

### Rejections and Status Codes

Every rejection is `401` with a `WWW-Authenticate: Bearer` challenge, except
`not_admitted`, which is `403`: the token verified; a different one will not help.

| `reason` label | Status | Meaning |
| --- | --- | --- |
| `no_token` | 401 | No bearer token in the `Authorization` header. |
| `unknown_issuer` | 401 | `iss` matches no configured issuer, or could not be read. |
| `bad_signature` | 401 | Did not verify against the issuer's keys, or is not a well-formed JWT. |
| `expired` | 401 | `exp` is in the past, or absent. |
| `not_yet_valid` | 401 | `nbf` is in the future by more than the clock-skew leeway. |
| `audience_mismatch` | 401 | `aud` does not contain the audience configured for that issuer. |
| `algorithm_not_allowed` | 401 | `alg` is not in that issuer's allow-list. |
| `jwks_stale` | 401 | The issuer's key set could not be refreshed and is no longer authoritative. |
| `not_admitted` | 403 | Token verified; its subject matches no admission rule for its issuer. |

The response body names only the reason. Issuer names and configuration detail
stay in NVKE's log. In permissive mode nothing is rejected: the decision is counted
under `outcome="would_reject"` with the status it would have returned.

### The Anonymous Surface

These need no token: `/healthz`, `/readyz`, `/api/version`, `/openapi.yaml`,
`/openapi-public.yaml`, `/docs`, and the Swagger UI under `/api/docs/`.
Everything else does, including paths that route nowhere, so an unauthenticated
caller cannot probe which ones exist. `auth/authn/routes.go` is the list.

## Debugging

### Local NICo/TPM Diagnostics

The TPM architecture for the mock-backed NICo flow is described above in
[TPM support in Docker](#tpm-support-in-docker). Use these checks when local
NICo `NicoMachine` objects get past instance creation but remain
`InstanceNotReady`, or when Docker node containers do not show `/dev/tpm0`.

Check CAPNICo and nico-sim first:

```bash
kubectl get nicomachine -A
kubectl logs -n capnico-system deploy/controller-manager
tilt logs nico-sim
docker ps --filter label=nvidia.com/nico-containers.managed=true
```

Check the Colima host-side vTPM proxy:

```bash
colima ssh -- sh -lc 'sysctl fs.inotify.max_user_instances; ls -l /dev/vtpmx'
```

Check a nico-containers-backed node container:

```bash
docker exec <container> bash -lc 'systemctl status swtpm-local.service'
docker exec <container> bash -lc 'journalctl -u swtpm-local.service --no-pager -n 80'
docker exec <container> bash -lc 'ls -l /sys/class/misc/vtpmx/dev /dev/vtpmx /dev/tpm* /var/lib/swtpm/ek.cert 2>&1'
```

Interpret the results this way:

- If `/dev/vtpmx` is missing, rerun `make host-up`. On macOS that is the Colima
  VM. On Linux it is the host kernel.
- If host-side CAPD e2e tests fail with `no route to host` to a `172.18.0.x`
  apiserver, confirm `docker-mac-net-connect` is installed and running
  (`sudo brew services list | grep docker-mac-net-connect`). See [First
  Run](#first-run).
- If the node container does not expose `/sys/class/misc/vtpmx/dev`,
  `swtpm-local.service` cannot create `/dev/vtpmx` or ask the kernel for
  `/dev/tpm0`.
- If `swtpm-local.service` fails, `spire-agent` usually fails by dependency.
- If `/var/lib/swtpm/ek.cert` is missing, nico-sim reports the instance as
  pending, and CAPNICo reports `InstanceNotReady`.
- Existing `nico-local-*` Docker containers should be recreated after fixing
  Colima or node-image TPM setup.

### Cursor/VS Code with Tilt-Managed Dependencies

Use this when you want to set breakpoints and step through CWE code while the rest of the stack (Temporal, Postgres, SPIRE) is managed by Tilt.

**Prerequisites:** `tilt up` is running and all resources are healthy. Make a copy of the local config that uses the Tilt deployment's Temporal namespace and queue:

```bash
mkdir -p .local
yq '.temporal.namespace = "nvke-controller" | .temporal.taskQueue = "nvke-controller-tasks"' hack/local-controller-config.yaml > .local/controller-config.tilt.yaml
```

and ensure `.env.local` points at it and at the Tilt-forwarded database:

```bash
export CONTROLLER_CONFIG_PATH="$PWD/.local/controller-config.tilt.yaml"
export DB_CONNECTION_STRING="postgres://postgres:postgres@localhost:5432/nvke-controller?sslmode=disable"
export DB_MAX_OPEN_CONNECTIONS=12
```

**Recommended workflow (Cursor/VS Code):**

1. Open **Run and Debug** in Cursor/VS Code (`Cmd+Shift+D`).
2. Select **"Launch CWE (Tilt Local Debug)"**.
3. Start debugging.
4. When you are done debugging, re-enable the in-cluster `nvke-controller` Tilt resource manually in the UI or with:

```bash
tilt trigger nvke-controller
```

This shared debug configuration:
- scales the in-cluster CWE deployment down to `0`
- starts the local SPIRE agent
- runs CWE locally under the debugger using `.env.local`
- prevents Tilt from redeploying in-cluster CWE while the local debug session is active

Set breakpoints in any `.go` file and they will be hit as Temporal dispatches workflows and activities.

The shared debug workflow is checked into:
- `.vscode/launch.json`
- `.vscode/tasks.json`

These are intended for repo-wide workflows. If you want personal debug variations, prefer a local untracked `*.code-workspace` file.

## Contributing

If you make a change to the OpenAPI spec, regenerate the generated Go files with:
```bash
make generate
```

Auto-update license header at the top of code files using:
```bash
make license
```
