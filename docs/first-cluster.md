# Your first cluster

NKE is a management plane. A **site controller** — one durable Kubernetes
cluster, also called a cell — mints **tenant clusters** on demand, each with its
own control plane, etcd and nodes. `nkectl` is how you drive it.

Three paths. Start with A.

| Path | Needs | Gets you |
|---|---|---|
| [**A — the API alone**](#path-a--the-api-alone) | Go, Docker, kind | CWE running and its API to explore, in minutes. No tenant clusters. |
| [**B — a tenant cluster with mock NICo**](#path-b--a-tenant-cluster-with-mock-nico) | Docker, kind, ~16 GB free, a CAPNICo checkout | The real create-a-cluster loop against real CAPNICo, no hardware |
| [**C — a real site**](#path-c--a-real-site) | Imaged nodes, nerdctl, a NICo site | A site controller minting CAPNICo-backed tenant clusters |

## Before you start

```bash
git clone https://github.com/dsx-ai-factory/nke.git
cd nke
```

| Tool | Needed for | Notes |
|---|---|---|
| [Go](https://go.dev/dl/) 1.26.6+ | all | `go.work` pins it. An older Go fetches the right toolchain unless `GOTOOLCHAIN=local` |
| [Docker](https://docs.docker.com/get-docker/) 24+ | A and B | Not supported for path C bootstrapping |
| [nerdctl](https://github.com/containerd/nerdctl) with containerd | C | The bootstrap drives kind and pulls images through it; no Docker daemon |
| GNU Make 4.0+ | all | macOS ships 3.81; `brew install make` gives `gmake` |
| [temporal](https://docs.temporal.io/cli) | A | `brew install temporal`, or `curl -sSf https://temporal.download/cli.sh \| sh` |
| [kind](https://kind.sigs.k8s.io/), `kubectl` | A and B | |
| `jq` | B | `dev-check` exits 1 without it |
| [helm](https://helm.sh/), [tilt](https://docs.tilt.dev/install.html) | B | |
| [clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start) | B | `make capd-up` installs it into `controller/.local/bin` |
| `dpkg-deb` | B on macOS | `brew install dpkg`; the bundle mirror build needs it |
| [Flox](https://flox.dev/) | optional | Pins the whole toolchain; never required |

macOS or Linux; Windows is not supported.

**On macOS, use Colima, not Docker Desktop.** `controller/Makefile.dev` points
every `kind`, `docker` and `clusterctl` call at the Colima socket, so `capd-up`
cannot work under Docker Desktop. A plain `colima start` gives a 2 CPU, 2 GiB VM
that `colima-up` will then refuse to resize, so size it first:

```bash
make -C controller colima-up      # 6 CPU, 12 GiB
```

Path A needs nothing internal and no NGC account. Path B needs a checkout of
`dsx-ai-factory/cluster-api-provider-nico`.

## Path A — the API alone

CWE against a local Postgres and Temporal, with a self-signed token instead of
NGC authentication. No tenant clusters, no hardware.

```bash
# Any cluster will do. CWE watches one and exits at startup without a kubeconfig.
kind create cluster

# Postgres, Temporal, then CWE.
make -C controller run-local
```

- Swagger — <http://localhost:8080/api/docs/>. Keep the trailing slash.
- `curl http://localhost:8080/api/clusters` → `{"clusters":[]}`. No token here.
- Temporal — <http://localhost:8233>.

## Path B — a tenant cluster with mock NICo

The same stack a real site runs: real CAPNICo, with **nico-mock** answering the
NICo API and tenant nodes as containers.

Tilt installs the released CAPNICo chart by default. To build CAPNICo from a
checkout instead, set `capnico_source_dir` in `controller/tilt_config.json` or
export `CAPNICO_SOURCE_DIR`.

If you ran path A first, stop it — its Postgres and Temporal hold 5432 and 7233,
and Tilt's probes will pass against the wrong servers while its port-forwards
retry forever:

```bash
make -C controller dev-stop-local
kind delete cluster            # the plain cluster from path A
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

### Driving it with nkectl

Point the CLI at the site controller:

```bash
go build -o bin/nkectl ./cli/cmd/nkectl

bin/nkectl config set-site --name local \
  --url http://localhost:8080 \
  --kubeconfig ~/.kube/config
bin/nkectl config view
```

`--kubeconfig` is the management cluster. `get providers` reads it directly, and
CWE API commands (`get`/`create`/`patch`/`delete` clusters) use it to mint a
bearer token for `nke-clients/nke-client` when authentication is on. Tilt
applies that identity in `controller/deploy/k8s/nke-auth-dev.yaml`. You do
not export a token for `nkectl`.

Under Tilt the API requires a token, unlike path A — a bare `curl` to
`/api/clusters` returns 401. `nkectl get clusters` does not, once `set-site`
has the kubeconfig: it discovers the audience from the `nke-auth-config`
ConfigMap and mints through the TokenRequest API.

```bash
bin/nkectl get providers      # wait for nico: CONFIGURED
bin/nkectl create cluster -f cli/examples/nico-mock-cluster.yaml
bin/nkectl get clusters
bin/nkectl get clusters --cluster <name>
bin/nkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: "1.31.0"'
bin/nkectl patch cluster my-cluster --patch-file patch.yaml
bin/nkectl delete cluster <name>
```

[`cli/examples/nico-mock-cluster.yaml`](../cli/examples/nico-mock-cluster.yaml)
works as-is — its UUIDs are nico-mock's seeded identifiers.

A node pool carries **either** a `nico` payload **or** an `infra` one, never
both. Path B uses `infra`, supplying the `NicoCluster` and `NicoMachineTemplate`
manifests directly; a real site uses `nico` and CWE builds them. Full schema at
`/api/docs`.

For `curl`, mint a token with `hack/nke-token.sh` after the stack is up:

```bash
TOKEN="$(controller/hack/nke-token.sh)"
curl -H "Authorization: Bearer ${TOKEN}" http://localhost:8080/api/clusters
```

`NKE_AUTH_ENABLED=false tilt up` turns authentication off. Then neither
`nkectl` nor `curl` needs a token — `nkectl` sees no `nke-auth-config`
ConfigMap and sends requests unauthenticated.

### Getting a tenant kubeconfig

```bash
nkectl get kubeconfig <cluster> --output-file tenant.kubeconfig
```

This reads the `<cluster>-admin-kubeconfig` Secret in `nke-system`, which
holds the verbatim `admin.conf`. The `-kubeconfig` Secret beside it is the one
CWE consumes, and it becomes a SPIRE exec-auth kubeconfig when OIDC is on.

## Path C — a real site

Real hardware, a real NICo site, and a site controller that outlives your
terminal. Steps 1 to 4 have been run end to end on leased bare metal, to a
`Ready` control-plane node; steps 5 to 7 have been run against mock NICo rather
than a real site. Treat a surprise here as a bug in this page and say so.

### Before you start

**Nodes.** An odd number — one, three, five; an even count is rejected. Each
already imaged and running `nke-agent` and `spire-agent`. You need every node's
IP and its **TPM EK thumbprint**: the site cluster attests nodes by TPM, and a
node with no thumbprint cannot join.

**kubeadm on each node, installed by your cloud-init.** An image built from
[`os-image/`](../os-image/) only *stages* the packages in a local apt repo at
`/usr/lib/nke/debs`; nothing installs them for a pre-provisioned node, because
CWE writes no cloud-init for hosts it did not create. Install them yourself:

```yaml
runcmd:
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nke-kubeadm-distro-<k8s-version>
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nke-cni-calico-images-<stream> nke-cni-calico-manifests-<stream>
```

Skip this and the bootstrap runs its full 30 minutes before failing on
`NodePreflight` / `check_cloud_init_status`, which never mentions kubeadm.

**A SPIRE CA.** A certificate and key you pass in the site spec, whose
certificate also reaches each node through cloud-init. Without it SPIRE mints a
new CA on every run that no node trusts.

**A SPIRE VIP pointed at the bootstrap node.** During the bootstrap the SPIRE
server runs on the machine you run `nkectl bootstrap site` from, published on
**30081**. Each node's `spire-agent` reaches it at the address you baked into its
configuration when you provisioned it, so **that VIP must point at the node you
run the bootstrap on.** Use a floating IP rather than the node's own address, so
it still resolves after bootstrapping completes.

**Network.** CWE must reach each node on **15005** and the new API server on
**6443**, and **30081** on nodes with a TPM thumbprint.

**Run the bootstrap on one of the provisioned bare metal machines**, under
`tmux` — not a laptop that sleeps. `nkectl` runs directly on that node; there is
no jump box.

**nerdctl with containerd**, on that machine. `nkectl` drives kind and pulls
every bootstrap image through nerdctl, and exports
`KIND_EXPERIMENTAL_PROVIDER=nerdctl` on every kind call itself. **Docker is not
supported for bootstrapping** — the machine needs no Docker daemon. **Do not run
nerdctl rootless.** The bootstrap sets up the kind node's networking — an
iptables rule and a bridge address — which needs a rootful nerdctl; run it as
root.

**A registry login** on that machine: `nerdctl login nvcr.io`. The images are
pulled with nerdctl.

**On the NICo side:** a site, a VPC, a subnet or VPC prefix, instance types, and
an **allocation** against each instance type. An instance type with no allocation
looks healthy and provisions nothing. See
[CAPNICo's getting started guide](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md).

### 1. Build nkectl

```bash
make -C cli build      # not `go build` — that leaves the Cluster API version unset
```

### 2. Generate the site spec

```csv
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,<sha256 of the node's TPM EK>
cp-02,10.10.50.3,<sha256 of the node's TPM EK>
cp-03,10.10.50.4,<sha256 of the node's TPM EK>
```

```bash
nkectl generate sitespec my-site --nodes hosts.csv \
  --set cluster.cniName=calico \
  --set spire.caCertFile=./spire-ca.crt \
  --set spire.caKeyFile=./spire-ca.key > sitespec.yaml

nkectl bootstrap site -f sitespec.yaml --dry-run   # validate, touch nothing
```

Read what it rendered. Unset fields get defaults that are silent rather than
absent — the Kubernetes version, `systemReservedMemory` at 16Gi, and a literal
`nvcr.io/org` in the registry block. Every field is in
[sitespec.md](../cli/docs/sitespec.md).

**Check `systemReservedMemory` against the node you actually have.** kubelet
computes allocatable as capacity minus reserved, so 16Gi on a smaller node
leaves nothing schedulable and the node never reaches `Ready`.

### 3. Point at your images

Both are required and the bootstrap fails without them. Outside a checkout you
also need `NKECTL_CWE_CHART_PATH` and `NKECTL_CAPI_PROVISIONED_PATH`, and those
two are not checked up front — a mistake surfaces after the seed cluster exists.

```bash
export NKECTL_CWE_IMAGE=<cwe-controller image>
export NKECTL_CAPI_PROVISIONED_IMAGE=<provisioned provider image>
```

### 4. Bootstrap

```bash
REGISTRY_API_KEY=<key> nkectl bootstrap site -f sitespec.yaml
```

The key can come from the environment, from `--registry-api-key`, or from
`registry.apiKey` in the spec; the flag wins.

Where Docker Hub is slow or blocked, point the seed cluster at a mirror:
`NKECTL_BOOTSTRAP_DOCKERHUB_MIRROR` and `NKECTL_CWE_POSTGRES_CHART`. The Postgres
chart pull is the step that times out. The Temporal and SPIRE charts have the
same knob — `NKECTL_CWE_TEMPORAL_CHART`, `NKECTL_CWE_SPIRE_CHART` and
`NKECTL_CWE_SPIRE_CRDS_CHART`, each an OCI reference or a local path — so every
Helm chart the bootstrap installs can come from a mirror or from disk.
`clusterctl init` still fetches Cluster API and cert-manager from `github.com`.
For the full set of third-party images
the bootstrap pulls — Postgres, Temporal, and the SPIRE stack — and to mirror
them into a local or private registry,
see [`hack/mirror-bootstrap-images.sh`](../hack/mirror-bootstrap-images.sh).

It creates a throwaway kind cluster, installs Postgres, Temporal, SPIRE, CAPI and
CWE into it, builds the site cluster on your hosts through `nke-agent`, and
writes `my-site.kubeconfig`. Allow 30 minutes.

On failure it writes `cluster-diagnostics/` first, and **leaves the seed cluster
behind** — delete it before retrying, or the next run stops on a name clash:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nkectl-bootstrap
```

The prefix is not optional: kind resolves a cluster through the provider that
created it, so a bare `kind delete cluster` looks for the seed cluster in
Docker, does not find it, and exits 0 without deleting anything — leaving the
name clash this step exists to prevent.

### 5. Install the site controller

The bootstrap leaves you a plain Kubernetes cluster. Installing CWE onto it is a
separate job, and the largest step here — `nkectl` has no command for it.

A plain kubeadm cluster is barer than a kind one, in two ways that stop the
install before it starts:

- **The nodes are all control plane**, so nothing schedules until you untaint
  them or give every workload a toleration:
  `kubectl taint nodes --all node-role.kubernetes.io/control-plane-`
- **There is no storage class.** The database and SPIRE both want a volume and
  sit `Pending` on `no persistent volumes available for this claim and no
  storage class is set`. Install a provisioner, or run them without persistence.

`nkectl get providers` is the quickest check that what you installed is wired
up — it names the missing piece rather than making you infer it.

Use the chart in
[`controller/deploy/charts/controller`](../controller/deploy/charts/controller).
It installs **CWE alone**, against a database and a Temporal you provide, and
defaults to a ClusterIP service with ingress off, so nothing reaches the API
until you change that. Install and configure the infrastructure provider
separately; the bootstrap does not carry its credentials over.

```bash
nkectl config set-site --name my-site --url https://<cwe-url> \
  --kubeconfig ./my-site.kubeconfig
```

That kubeconfig is also how `nkectl` authenticates to CWE once the site has API
authentication on. The CLI always mints for `nke-clients/nke-client` and reads
the audience from `cloud-workflow-engine/nke-auth-config`. Create that service
account, admit it, and grant the kubeconfig identity the permissions in
[Configuring NKE API Authentication](../controller/docs/configuring-api-authentication.md).
Without them, `get clusters` fails at minting or CWE returns 403.

### 6. Install CAPNICo

Tenant clusters need it. Install the provider onto the site cluster with a
`nico-credentials` Secret in `capnico-system` holding the endpoint, org and
**one** authentication mode. Prefer client credentials: a password-grant token
stops provisioning when it expires.
[CAPNICo's guide](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md)
is the reference.

```bash
nkectl get providers      # nico must read CONFIGURED before you create anything
```

### 7. Create a tenant cluster

```bash
nkectl create cluster -f nico-cluster.yaml
nkectl get clusters
```

Start from [`cli/examples/nico-cluster.yaml`](../cli/examples/nico-cluster.yaml).
Replace every UUID, `orgID`, and both profile names — a leftover placeholder
returns 500 rather than a validation error. The OS and config profiles it names
must exist on your site controller; profile content is maintained outside this
repository.

### Rough edges

- The site controller does not manage itself: no upgrade, node cycling or
  scaling.

## See also

- [getting-started.md](getting-started.md) — building and testing the modules
- [concepts.md](concepts.md) — the vocabulary, one page
- [architecture.md](architecture.md) — how CWE, the agent and CAPI fit together
- [../cli/docs/sitespec.md](../cli/docs/sitespec.md) — every site spec field
- [../cli/docs/user-flow.md](../cli/docs/user-flow.md) — the command reference
- [CAPNICo getting started](https://github.com/dsx-ai-factory/cluster-api-provider-nico/blob/main/docs/getting-started.md)
  — the infrastructure provider every real site runs
