# Your first cluster

NVKE is a management plane. A **management cluster** — one durable Kubernetes
cluster, also called a cell — mints **tenant clusters** on demand, each with its
own control plane, etcd and nodes. `nvkectl` is how you drive it.

Three paths — pick the one that matches what you have:

| Path | Who it's for | Needs | Gets you |
|---|---|---|---|
| [**Real Site**](#real-site) | **Operators** standing up a production NVKE site | Imaged nodes, nerdctl, a NICo site | A management cluster minting CAPNICo-backed tenant clusters |
| [**Mock NICo**](#mock-nico) | **Contributors** testing the full create-cluster loop without hardware | Docker, kind, ~16 GB free, and either a CAPNICo checkout or an NGC account | The real create-a-cluster loop against real CAPNICo, no hardware |
| [**API Only**](#api-only) | **Contributors** exploring the CWE REST API | Go, Docker, kind | CWE running and its API to explore, in minutes. No tenant clusters. |

## Before you start

```bash
git clone https://github.com/dsx-ai-factory/nvke.git
cd nvke
```

| Tool | Needed for | Notes |
|---|---|---|
| [Go](https://go.dev/dl/) 1.27.3+ | all | `go.work` pins it. An older Go fetches the right toolchain unless `GOTOOLCHAIN=local` |
| [Docker](https://docs.docker.com/get-docker/) 24+ | API Only, Mock NICo | Not supported for Real Site bootstrapping |
| [nerdctl](https://github.com/containerd/nerdctl) with containerd | Real Site | The bootstrap drives kind and pulls images through it; no Docker daemon |
| GNU Make 4.0+ | all | macOS ships 3.81; `brew install make` gives `gmake` |
| [temporal](https://docs.temporal.io/cli) | API Only | `brew install temporal`, or `curl -sSf https://temporal.download/cli.sh \| sh` |
| [kind](https://kind.sigs.k8s.io/), `kubectl` | all | Real Site: `nvkectl bootstrap site` runs both |
| `jq` | Mock NICo | `dev-check` exits 1 without it |
| [helm](https://helm.sh/) | Mock NICo, Real Site | `nvkectl bootstrap site` runs it for the Postgres, Temporal, SPIRE and CWE releases |
| [tilt](https://docs.tilt.dev/install.html) | Mock NICo | |
| [clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start) | Mock NICo, Real Site | `make capd-up` installs it into `controller/.local/bin`; `nvkectl bootstrap site` runs `clusterctl init` |
| `dpkg-deb` | Mock NICo (macOS) | `brew install dpkg`; the bundle mirror build needs it |
| [Flox](https://flox.dev/) | optional | Pins the whole toolchain; never required |

macOS or Linux; Windows is not supported.

**Prepare the Docker host once, before Kind or Tilt.** On macOS, use Colima
rather than Docker Desktop. `controller/Makefile.dev` points every `kind`,
`docker` and `clusterctl` call at the Colima socket, so `capd-up` cannot work
under Docker Desktop. A plain `colima start` gives a 2 CPU, 2 GiB VM that this
command then refuses to resize. On Linux the same command loads `tpm_vtpm_proxy`
on the host kernel, which container nodes need for a software TPM.

```bash
make -C controller host-up
```

**API Only** needs nothing internal and no NGC account.

**Mock NICo** installs CAPNICo one of two ways, and **only one of them works without an
NGC account**. Set `CAPNICO_SOURCE_DIR` to a checkout of
[`dsx-ai-factory/cluster-api-provider-nico`](https://github.com/dsx-ai-factory/cluster-api-provider-nico),
which is public, and it builds from source. Leave it unset and it installs the
released chart from NGC instead, which needs `NGC_API_KEY`. The source route is
the one to take if you are outside NVIDIA.

## API Only

CWE against a local Postgres and Temporal, with a self-signed token instead of
NGC authentication. No tenant clusters, no hardware.

```bash
# Any cluster will do. CWE watches one and exits at startup without a kubeconfig.
kind create cluster

# Postgres, Temporal, then CWE.
make -C controller run-local
```

- Swagger — http://localhost:8080/api/docs/ (keep the trailing slash)
- `curl http://localhost:8080/api/clusters` → `{"clusters":[]}`. No token here.
- Temporal — http://localhost:8233

## Mock NICo

The same stack a real site runs: real CAPNICo, with **nico-mock** answering the
NICo API and tenant nodes as containers.

Tilt installs the released CAPNICo chart by default, which pulls from NGC and so
needs `NGC_API_KEY`. To build CAPNICo from a checkout instead, and need no NGC
account, set `capnico_source_dir` in `controller/tilt_config.json` or export
`CAPNICO_SOURCE_DIR`.

If you ran **API Only** first, stop it — its Postgres and Temporal hold 5432 and 7233,
and Tilt's probes will pass against the wrong servers while its port-forwards
retry forever:

```bash
make -C controller dev-stop-local
kind delete cluster            # the plain cluster from API Only
```

```bash
cp controller/tilt_config.json.example controller/tilt_config.json

# Credentials. Tilt fails fast without all five; local values are fine.
cp controller/.env.tilt.example controller/.env.tilt
# then edit controller/.env.tilt and fill the five empty values

# Only if the CAPNICo checkout is not a sibling of this repository. Absolute:
# Tilt runs the provider script from controller/, so a relative path resolves
# inside the repository.
export CAPNICO_SOURCE_DIR="$PWD/../cluster-api-provider-nico"

# kind cluster, Cluster API, and the bundle mirror the node image build reads.
# Tilt checks for all three and refuses to start without them.
make -C controller capd-up

cd controller && tilt up      # leave running
```

That config selects `["capi_core", "bgp-containers", "nico"]`, the list the
[NICo E2E lane](../.github/workflows/nico-e2e.yml) runs. The single `nico`
provider starts nico-sim and installs one CAPNICo controller in
`capnico-system`.

[`controller/README.md`](../controller/README.md) covers the Tilt loop in full.

### Driving it with nvkectl

Point the CLI at the management plane:

```bash
go build -o bin/nvkectl ./cli/cmd/nvkectl

bin/nvkectl config set-site --name local \
  --url http://localhost:8080 \
  --kubeconfig ~/.kube/config
bin/nvkectl config view
```

`--kubeconfig` is the management cluster. `get providers` reads it directly, and
CWE API commands (`get`/`create`/`patch`/`delete` clusters) use it to mint a
bearer token for `nke-clients/nke-client` when authentication is on. Tilt
applies that identity in `controller/deploy/k8s/nvke-auth-dev.yaml`. You do
not export a token for `nvkectl`.

Under Tilt the API requires a token, unlike **API Only** — a bare `curl` to
`/api/clusters` returns 401. `nvkectl get clusters` does not, once `set-site`
has the kubeconfig: it discovers the audience from the `nvke-auth-config`
ConfigMap and mints through the TokenRequest API.

```bash
bin/nvkectl get providers      # wait for nico: CONFIGURED
bin/nvkectl create cluster -f cli/examples/nico-mock-cluster.yaml
bin/nvkectl get clusters
bin/nvkectl get clusters <name>
bin/nvkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: "1.31.0"'
bin/nvkectl patch cluster my-cluster --patch-file patch.yaml
bin/nvkectl delete cluster <name>
```

[`cli/examples/nico-mock-cluster.yaml`](../cli/examples/nico-mock-cluster.yaml)
works as-is — its UUIDs are nico-mock's seeded identifiers.

The cluster's top-level `nico` or `infra` payload sets its provider, and the
control plane and every node pool must carry the same one. **Mock NICo** uses
`infra`, supplying the `NicoCluster` and `NicoMachineTemplate` manifests
directly; a real site uses `nico` and CWE builds them. See
[Control plane configuration](#control-plane-configuration); full schema at
`/api/docs`.

For `curl`, mint a token with `hack/nvke-token.sh` after the stack is up:

```bash
TOKEN="$(controller/hack/nvke-token.sh)"
curl -H "Authorization: Bearer ${TOKEN}" http://localhost:8080/api/clusters
```

`NKE_AUTH_ENABLED=false tilt up` turns authentication off. Then neither
`nvkectl` nor `curl` needs a token — `nvkectl` sees no `nvke-auth-config`
ConfigMap and sends requests unauthenticated.

### Getting a tenant kubeconfig

```bash
nvkectl get kubeconfig <cluster> --output-file tenant.kubeconfig
```

This reads the `<cluster>-admin-kubeconfig` Secret in `nvke-system`, which
holds an `nvke:breakglass` admin kubeconfig. The `-kubeconfig` Secret beside
it is the one CWE consumes, and it becomes a SPIRE exec-auth kubeconfig when
OIDC is on.

## Real Site

Real hardware, a real NICo site, and a management cluster that outlives your
terminal. Steps 1 to 4 have been run end to end on leased bare metal to a
`Ready` control-plane node; steps 5, 6 and 8 have been run against mock NICo
rather than a real site, and step 7 against neither. New management cluster
clusters use OVN-Kubernetes. Treat a surprise here as a bug in this page and
say so.

### Before you start

**Nodes.** An odd number — one, three, five; an even count is rejected. Each
already imaged and running `nvke-agent` and `spire-agent`. You need every node's
IP and its **TPM EK thumbprint**: the site cluster attests nodes by TPM, and a
node with no thumbprint cannot join.

**kubeadm on each node, installed by your cloud-init.** An image built from
[`os-image/`](https://github.com/dsx-ai-factory/nvke/tree/main/os-image) only *stages* the packages in a local apt repo at
`/usr/lib/nvke/debs`; nothing installs them for a pre-provisioned node, because
CWE writes no cloud-init for hosts it did not create. Install them yourself:

```yaml
runcmd:
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-kubeadm-distro-<k8s-version>
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-cni-ovn-images-<stream> nvke-cni-ovn-charts-<stream>
```

Skip this and the bootstrap runs its full 30 minutes before failing on
`NodePreflight` / `check_cloud_init_status`, which never mentions kubeadm.

**A SPIRE CA.** A certificate and key you pass through
`NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE`, with
`spire.upstreamCA: true` in the site spec, whose certificate also reaches each
node through cloud-init. Without it SPIRE mints a
new CA on every run that no node trusts.

**A SPIFFE trust domain, when bootstrapping with SPIRE/SPIFFE enabled.** Set
`spire.trustDomain` in the site spec. It must match the `trust_domain` already
configured on every host's SPIRE agent and the trust domain that the permanent
site SPIRE server will use. Do not rely on the bootstrap cluster name to select
this value: changing it after bootstrap changes every SPIFFE ID and prevents
the existing agents and workload entries from moving to the permanent server.

**A SPIRE VIP pointed at the bootstrap node.** During the bootstrap the SPIRE
server runs on the machine you run `nvkectl bootstrap site` from, published on
**30081**. Each node's `spire-agent` reaches it at the address you baked into its
configuration when you provisioned it, so **that VIP must point at the node you
run the bootstrap on.** Use a floating IP rather than the node's own address, so
it still resolves after bootstrapping completes.

**Network.** CWE must reach each node on **15005** and the new API server on
**6443**; nodes with a TPM thumbprint must reach this machine on **30081**, the fixed NodePort `nvkectl` publishes the bootstrap SPIRE server on.

**Run the bootstrap on one of the provisioned bare metal machines**, under
`tmux` — not a laptop that sleeps. `nvkectl` runs directly on that node; there is
no jump box.

**nerdctl with containerd**, on that machine. `nvkectl` drives kind and pulls
every bootstrap image through nerdctl, and exports
`KIND_EXPERIMENTAL_PROVIDER=nerdctl` on every kind call itself. **Docker is not
supported for bootstrapping** — the machine needs no Docker daemon. **Do not run
nerdctl rootless.** `nvkectl` and kind's nerdctl provider both exec a bare
`nerdctl` with no `sudo`, and rootful is the only configuration CI exercises;
run it as root.

**A registry login** on that machine: `nerdctl login nvcr.io`. The images are
pulled with nerdctl.

**On the NICo side:** a site, a VPC, a subnet or VPC prefix, instance types, and
an **allocation** against each instance type. An instance type with no allocation
looks healthy and provisions nothing. See
[CAPNICo's getting started guide](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md).

### 1. Build nvkectl

```bash
make -C cli build      # not `go build` — that leaves the Cluster API version unset
export PATH="$PWD/cli/bin:$PATH"   # the binary lands in cli/bin, not on PATH
```

The rest of this path calls `nvkectl` unqualified, so either export the path as above
or invoke it as `./cli/bin/nvkectl`.

`nvkectl` also shells out to `helm`, `kubectl` and `clusterctl` during bootstrap, on
top of the nerdctl and containerd below. Only the container runtime is checked before
the run starts, so a missing one of the other three surfaces mid-bootstrap, after the
Kind cluster is already up.

### 2. Generate the site spec

```csv
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,<sha256 of the node's TPM EK>
cp-02,10.10.50.3,<sha256 of the node's TPM EK>
cp-03,10.10.50.4,<sha256 of the node's TPM EK>
```

```bash
nvkectl generate sitespec my-site --nodes hosts.csv \
  --set cluster.controlPlaneVIP=10.10.50.10 \
  --set cluster.podCidr=10.244.0.0/16 \
  --set spire.trustDomain=site.example.com \
  --set spire.upstreamCA=true > sitespec.yaml

# The SPIRE CA comes from the environment.
export NVKECTL_SPIRE_CA_CERT_FILE=./spire-ca.crt NVKECTL_SPIRE_CA_KEY_FILE=./spire-ca.key
nvkectl bootstrap site -f sitespec.yaml --dry-run   # validate, touch nothing
```

`cluster.controlPlaneVIP` is advertised over BGP, so the network must peer
with the nodes. See [sitespec.md](../cli/docs/sitespec.md).

Read what it rendered. Unset fields get defaults that are silent rather than
absent — the Kubernetes version and `systemReservedMemory` at 16Gi. Every field is in
[sitespec.md](../cli/docs/sitespec.md).

**Check `systemReservedMemory` against the node you actually have.** kubelet
computes allocatable as capacity minus reserved, so 16Gi on a smaller node
leaves nothing schedulable and the node never reaches `Ready`.

### 3. Point at your images

`make -C cli images` builds both with nerdctl on this host, tagged
`nke-controller:latest` and `cluster-api-provider-provisioned:latest`, and the
bootstrap side-loads them by default. To use images you pushed to a registry
instead, name them:

```bash
export NVKECTL_NVKE_CONTROLLER_IMAGE=<nvke-controller image>
export NVKECTL_CAPI_PROVISIONED_IMAGE=<provisioned provider image>
```

The `--nvke-controller-image` and `--provisioned-provider-image` flags do the same for one
run. `nvkectl` carries the CWE chart and the provisioned provider's manifests.
Step 4 lists what it fetches.

### 4. Bootstrap

```bash
nvkectl bootstrap site -f sitespec.yaml
```

To use images from a private registry, log in to it first with
`nerdctl login <registry>`; `nvkectl` pulls them with nerdctl.

`nvkectl get bootstrap-manifest` lists the charts and images the bootstrap
fetches. It leaves out the kind node image and what `clusterctl init` installs.
[`hack/mirror-bootstrap-images.sh`](../hack/mirror-bootstrap-images.sh) copies
the listed images into a local or private registry. To change where they come
from:

- `NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR=<host>` pulls the Docker Hub images from a mirror.
- `NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART`, `NVKECTL_NVKE_CONTROLLER_SPIRE_CHART` and
  `NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART` take an OCI reference or a local path.

Images from `ghcr.io`, `registry.k8s.io` and `cgr.dev` come from those
registries. Cluster API and cert-manager come from `github.com` through
`clusterctl init`.

It creates a throwaway kind cluster, installs Postgres, Temporal, SPIRE, CAPI and
CWE into it, builds the site cluster on your hosts through `nvke-agent`, and
writes `my-site.kubeconfig` (a breakglass kubeconfig in group `nvke:breakglass`).
Allow 30 minutes.

Allow longer than that and raise `--timeout` (default `30m`) if the site is large —
the command gives up on the provisioning wait when it expires.

On failure it writes diagnostics to `cluster-diagnostics/` (override with
`--diagnostics-dir`). It then **keeps the seed cluster only if the failure came after
the site cluster was created**, or if you passed `--keep-bootstrap-cluster`; an earlier
failure — a failed image pull, an unhealthy CWE — tears it down.
When it is kept, `nvkectl` prints the delete command. Run it before retrying, once
anything you need from the seed cluster is saved: a leftover cluster makes the next
run refuse to start, and its message leads with saving the etcd encryption key.

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap
```

The prefix is not optional: kind resolves a cluster through the provider that
created it, so a bare `kind delete cluster` looks for the seed cluster in
Docker, does not find it, and exits 0 without deleting anything — leaving the
name clash this step exists to prevent.

[Troubleshooting a Site Bootstrap](../cli/docs/bootstrap-troubleshooting.md)
covers what is in `cluster-diagnostics/`, how to read the error, and the
common failures with their fixes.

### 5. Install an infrastructure provider

Cluster API and CAPNICo are part of the next step: the install's list of
commands installs Cluster API with `clusterctl init` and CAPNICo from the
`deploy/nico` deploy path, in that order and before CWE. Do not install them
separately first; [Configure an infrastructure provider: NICo](operate/provider-nico.md)
explains how to prove the NICo credentials once the site is up.

### 6. Install the management plane

The bootstrap leaves you a plain Kubernetes cluster. Installing CWE onto it is a
separate job, and the largest step here — `nvkectl` has no command for it.

Follow [Run NVKE end to end on one machine](get-started/local-site.md).
It installs the whole management plane, Postgres, Temporal, SPIRE, CAPNICo,
NVKE telemetry pipeline (NKTP) and CWE, with the commands that page lists, from the `deploy/nico`
deploy path, the site spec and an NVKR release description.

```bash
nvkectl config set-site --name my-site --url http://<VIP>:30080 \
  --kubeconfig ./my-site.kubeconfig
```

Port 30080 is the plain-HTTP NodePort the chart exposes, and the CLI sends a
bearer token to it, so use it from a trusted network only.

That kubeconfig is also how `nvkectl` authenticates to CWE once the site has API
authentication on. The CLI always mints for `nke-clients/nke-client` and reads
the audience from ConfigMap `nvke-auth-config` in namespace `nvke-controller`,
then in `nke-controller` for a site installed before the `nvke` rename, then in
`cloud-workflow-engine`, and uses the first it can read. Create that service
account, admit it, and grant the kubeconfig identity the permissions in
[Configuring NVKE API Authentication](../controller/docs/configuring-api-authentication.md).
Without them, `get clusters` fails at minting or CWE returns 403.

With the site registered, `nvkectl` can report on the provider the install put
in place:

```bash
nvkectl get providers      # nico must read CONFIGURED before you create anything
```

### 7. Publish a release

Step 6 ended by publishing a release through the install guide. To add another,
for a second image or a changed user data, publish a new set under a new name;
`nvkectl create` refuses an existing one.

Copy the three examples —
[`machineimage.yaml`](../cli/examples/machineimage.yaml),
[`clusterrelease.yaml`](../cli/examples/clusterrelease.yaml) and
[`nodepoolrelease.yaml`](../cli/examples/nodepoolrelease.yaml) — and give them a
name of your own, one name across all three:

```bash
cp cli/examples/machineimage.yaml cli/examples/clusterrelease.yaml \
   cli/examples/nodepoolrelease.yaml .
```

Set `metadata.name` in all three, and `spec.machineImageRef.name` in the two
releases to match. Edit the rest for your image, then publish, image first:

```bash
nvkectl create machineimage -f machineimage.yaml
nvkectl create clusterrelease -f clusterrelease.yaml
nvkectl create nodepoolrelease -f nodepoolrelease.yaml
nvkectl get machineimages      # the name you just published
```

That name is what a cluster asks for as its `release`. Published content is
immutable, so changing a release means publishing a new name.

### 8. Create a tenant cluster

```bash
nvkectl create cluster -f nico-cluster.yaml
nvkectl get clusters
```

Start from [`cli/examples/nico-cluster.yaml`](../cli/examples/nico-cluster.yaml).
Replace every UUID and `orgID` — a leftover placeholder returns 500 rather than
a validation error. Every `release` has to name a release already published on
the site — the one from step 7. Every `configProfile` has to name a config
profile the management plane already holds, and that content is maintained
outside this repository.

### Decommissioning

`nvkectl delete cluster <name>` starts an asynchronous teardown. There is no
confirmation prompt and no wait flag, so follow it with `nvkectl get clusters`.

Teardown removes the node pools, the nodes, the Cluster API and infrastructure
objects, and NVKE's own records, then asks the infrastructure provider to release
each machine. What happens to the hardware after that release is the provider's
decision, not NVKE's.

**Teardown does not clean the hosts.** The only action NVKE takes on a node is a
best-effort `kubeadm reset -f`, and a failure there does not stop the teardown.
The node agent, the SPIRE agent, kubeadm, kubelet and containerd all stay
installed, so reclaiming a host for something else is manual:

```bash
apt-get purge nvke-agent spire-agent
```

A purge still leaves `/var/lib/nvke/state.json` and the two agent logs on disk.

**No command tears down a site cluster.** `nvkectl delete` covers tenant clusters
and node pools only. Reversing `nvkectl bootstrap site` is manual.

**Clusters built on pre-provisioned hosts are not released.** The provisioned
provider refuses to delete a bound machine, because it cannot make the host safe
for reuse. Destroy the bootstrap management cluster instead.

Once a cluster is marked Deleting, a later node or node-pool delete returns 202
and does nothing, and a failed teardown leaves that mark in place. A stuck
teardown cannot be helped along through the API.

### Rough edges

- The management cluster does not manage itself: no upgrade, node cycling or
  scaling.

## Control plane configuration

A tenant cluster's control plane is described once, at the cluster level, by
`controlPlaneConfig`. The management plane's control-plane controller manages
it: it creates control-plane hosts in parallel, joins them to the cluster one
at a time, and deletes them one at a time. A create request carries
`controlPlaneConfig` alongside a `nodePools` list that holds only workers:

```yaml
controlPlaneConfig:
  desiredCount: 3            # >= 1; keep it odd so etcd holds a quorum
  maxSurge: 1                # 0 or 1: 1 replaces before removing, 0 removes first
  systemReservedMemory: "16Gi"
  nico:
    instanceTypePreference:  # ordered UUIDs; index 0 is tried first, the rest are fallback
      - 00000000-0000-0000-0000-000000000000
    vpcPrefixIDs:
      - 00000000-0000-0000-0000-000000000000
    sshKeyGroupIDs:
      - 00000000-0000-0000-0000-000000000000
    release: nke-260625-24.04-x86-fde
    configProfile: default-cp
```

`controlPlaneConfig` takes exactly one provider payload, `nico` or `infra`,
matching the cluster's own. [`nico-cluster.yaml`](../cli/examples/nico-cluster.yaml),
[`nico-mock-cluster.yaml`](../cli/examples/nico-mock-cluster.yaml) and
[`capd-cluster.yaml`](../cli/examples/capd-cluster.yaml) are complete requests.

A node pool with `controlPlane: true` is rejected.

## See also

- [getting-started.md](getting-started.md) — building and testing the modules (contributor / local dev)
- [concepts.md](concepts.md) — the vocabulary, one page
- [architecture.md](architecture.md) — how CWE, the agent and CAPI fit together
- [../cli/docs/sitespec.md](../cli/docs/sitespec.md) — every site spec field
- [../cli/docs/user-flow.md](../cli/docs/user-flow.md) — the command reference
- [operate/provider-nico.md](operate/provider-nico.md)
  — proving the NICo credentials and what CAPNICo's states mean
