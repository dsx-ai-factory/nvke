# Site and Tenant Cluster Flow

What `nvkectl` does today, in the order an operator uses it, with the two
steps that are not CLI commands marked: getting a release from NVKR and
installing the controller stack with the deploy script.
The [command reference](https://github.com/dsx-ai-factory/nvke/blob/main/cli/docs/reference/README.md)
lists every command and flag; it is generated from the CLI, so it says the same
as `nvkectl <command> --help`. The
walkthrough is [Your first cluster](https://github.com/dsx-ai-factory/nvke/blob/main/docs/first-cluster.md); this page is
the reference it points at.

## Commands

| Command | What it does | Status |
|---|---|---|
| `nvkectl generate sitespec <name> --nodes hosts.csv` | Writes a site spec for `bootstrap site` from a node CSV. `--set path=value` fills in what the CSV does not carry. | Available |
| `nvkectl bootstrap site -f sitespec.yaml` | Stands up an ephemeral Kind cluster running the NVKE controller, has the controller create the site cluster on the spec's hosts through `nvke-agent`, writes `<name>.kubeconfig` and `<name>-etcd-encryption-key.json`, tears the Kind cluster down on success. `--dry-run` validates the spec only. | Available |
| `nvkectl validate sitespec -f sitespec.yaml` | Checks the site spec the way `bootstrap site` reads it and prints `valid`. | Available |
| Write the stack config, `stack-config.json` | The controller stack install reads it: the NICo API, and under `images` the profile content image (required without a release) and, optionally, the controller image. Copy `deploy/examples/stack-config.json` and fill in every `REPLACE_*` value; the install refuses a placeholder. The tokens and passwords come from `NVKECTL_*` variables. Not read by `bootstrap site`. | Available |
| `nvkectl get bootstrap-manifest` | Prints the charts and images `bootstrap site` fetches, with version and digest. | Available |
| `nvkr release verify` and `nvkr release export --reference <release>` | NVKR, not `nvkectl`: verifies a published release under your policy, then writes its description, the `--release` input of the install. Optional: without it the charts come from this checkout, the profile content image from the stack config's `images.profiles`, and the controller image from `images.controller` or, when that is empty, the deploy path's pin. | Not in the CLI |
| `clusterctl init`, `deploy/nvke-deploy-local.sh install` | The management plane install, a short list of commands run in order: `clusterctl init` installs Cluster API; the wrapper installs the lab StorageClass, renders the `deploy/nico` deploy path for the site (Postgres, Temporal, SPIRE, CAPNICo, NKTP, the controller) and applies it, and gives the Cluster API controllers SPIRE exec-auth. See [Run NVKE end to end on one machine](../../docs/get-started/local-site.md). | Not in the CLI |
| `nvkectl config set-site --url <controller-url> --kubeconfig <path>` | Points the CLI at an NVKE management plane. `use-site` switches between entries, `view` prints the config. | Available |
| `nvkectl get providers` | Lists the Cluster API infrastructure providers the management plane can use, with their state. Alpha. | Available |
| `nvkectl create cluster -f spec.yaml` | Creates a tenant cluster from a `CreateClusterRequest` YAML. `-f -` reads stdin. | Available |
| `nvkectl get clusters [name ...]` | Lists tenant clusters. Names narrow to those clusters. `-o json\|yaml` changes the format. | Available |
| `nvkectl get kubeconfig <name>` | Fetches a tenant cluster's admin kubeconfig. `--output-file` writes it to a file instead of stdout. | Available |
| `nvkectl delete cluster <name>` | Deletes a tenant cluster. | Available |
| `nvkectl patch cluster <name>` | PATCHes a mutable cluster property (Kubernetes version, etcd encryption, remote access, control plane config). `--patch` takes an inline YAML document, `--patch-file` a file (or stdin via `-`). | Available |
| `nvkectl create nodepool -f spec.yaml` | Adds a node pool to an existing cluster from a `CreateNodePoolRequest` YAML. `-f -` reads stdin. | Available |
| `nvkectl get nodepools [name ...]` | Lists node pools. Names narrow to those pools, `--cluster <name>` to one cluster. `-o json\|yaml` changes the format. | Available |
| `nvkectl delete nodepool <name> --cluster <name>` | Removes a node pool from a cluster. A cluster has a control-plane pool only on a management plane with the control-plane controller off; deleting its last joined one is rejected, so delete the cluster instead. | Available |
| `nvkectl patch nodepool <name> --cluster <name>` | PATCHes a node pool. `--patch` takes an inline YAML document, `--patch-file` a file (or stdin via `-`). `desiredNodeCount` is the only field supported so far. | Available |
| `nvkectl version` | Prints version, commit and build date. `-o short\|yaml\|json`. | Available |

`cluster` and `clusters` are aliases on `create`, `get`, `patch` and `delete`,
as are `nodepool` and `nodepools` on `create`, `get`, `patch` and `delete`.
Global flags: `--site <name>` overrides the active site for one invocation,
`-v` enables debug logging, `--json-logs` emits logs as JSON.

## Example

### 1. Get a release

NVKR builds and publishes NVKE's artifacts. Verify a published release under
your policy, then export its description; the install takes that file as
`--release` and pulls every chart and image it carries by digest. The
release also carries `nvkectl` itself; [Bootstrap the management cluster](../../docs/operate/bootstrap-management-cluster.md#install-nvkectl)
shows how to take it from there.

```bash
nvkr release verify --reference <repo@digest> --lane pipeline-attested --scope artifact-publication \
  --identity <signer identity regexp> --issuer <OIDC issuer> --cosign <cosign> --cosign-digest <sha256>
nvkr release export --reference <repo@digest> > release.json
```

`export` describes the release and verifies nothing, so run it only on a
reference `verify` accepted. [Get NVKE artifacts from NVKR and write release manifests](../../docs/operate/build-node-images.md)
explains what each command checks and writes. The release
is optional: without it the install takes the charts from this checkout and
their public repositories, the stack config names the profile content image
in `images.profiles`, and `images.controller` may override the deploy path's
pinned controller image.

### 2. Describe the hosts and generate the site spec

A CSV with a header names the hosts. `name` and `ip` are required;
`tpm_ek_thumbprint` is optional per row and turns on TPM attestation for that
host.

```csv
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,
cp-02,10.10.50.3,
cp-03,10.10.50.4,
```

```bash
nvkectl generate sitespec my-tier0-site --nodes hosts.csv > sitespec.yaml

# Override fields the CSV does not carry
nvkectl generate sitespec my-tier0-site --nodes hosts.csv \
  --set cluster.controlPlaneVIP=10.10.50.10 \
  --set cluster.podCidr=10.244.0.0/16 > sitespec.yaml
```

The file format is described in [sitespec.md](sitespec.md); a complete example
is in [examples/sitespec.yaml](https://github.com/dsx-ai-factory/nvke/blob/main/cli/examples/sitespec.yaml).

### 3. Bootstrap the site cluster

```bash
# Validate only: load, check and print the spec, touch nothing
nvkectl bootstrap site -f sitespec.yaml --dry-run

# Bootstrap
nvkectl bootstrap site -f sitespec.yaml
```

The command needs nerdctl and containerd on the host, plus `kind`, `helm`,
`kubectl` and `clusterctl` on `PATH`, and network reach to the hosts'
`nvke-agent` ports. It leaves a plain Kubernetes cluster and
`my-tier0-site.kubeconfig`.

### 4. Write the stack config

The install takes what only the stack needs from a file of its own, beside
the site spec, and the credentials from the environment:

```bash
cp deploy/examples/stack-config.json stack-config.json
read -rs NVKECTL_NICO_TOKEN && export NVKECTL_NICO_TOKEN
export NVKECTL_NODE_USER=nvke
read -rs NVKECTL_NODE_PASSWORD && export NVKECTL_NODE_PASSWORD
read -rs NVKECTL_DATABASE_PASSWORD && export NVKECTL_DATABASE_PASSWORD
```

Fill in every `REPLACE_*` value: the NICo API endpoint, organization ID and
API name. Without a release, name the profile content image in
`images.profiles`. The file holds no secret, so it can sit next to the site
spec. [deploy/README.md](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/README.md) lists every key.

### 5. Install the controller stack

```bash
clusterctl init --kubeconfig local-site.kubeconfig --core cluster-api:<v> --bootstrap kubeadm:<v> --control-plane kubeadm:<v> --wait-providers
"$NVKE_REPO"/deploy/nvke-deploy-local.sh install
kubectl --kubeconfig local-site.kubeconfig apply -f nvke-api-clients.yaml
```

A list of commands, in this order: `clusterctl init` installs Cluster API at
the pinned version; the wrapper installs the lab StorageClass, reads the
nodes' architecture from the cluster, renders and applies the `deploy/nico`
deploy path item by item, and patches the Cluster API controllers for SPIRE;
the NetworkPolicy admits your client
networks to the controller API, which the chart otherwise answers from
in-cluster pods only. Then point the CLI at the site (step 6) and publish a `MachineImage`, `ClusterRelease` and `NodePoolRelease`
with `nvkectl create`, as [Your first cluster](https://github.com/dsx-ai-factory/nvke/blob/main/docs/first-cluster.md)
shows, before the first tenant cluster.
[Run NVKE end to end on one machine](../../docs/get-started/local-site.md)
gives each step with its inputs.

The deploy path is the interface and the script is the reference way to apply
it. `deploy/nico/nvke-deploy.yaml` lists the items in install order, and each
item directory holds its chart pin, values and overlay, or its manifests;
`--dry-run` prints every kubectl and helm call the script would make. An operator can apply the same items with their own tooling or through
GitOps; [Deploy Path](https://github.com/dsx-ai-factory/nvke/blob/main/docs/site-bootstrap/deploy-repository.md)
describes the format.

### 6. Point the CLI at the management plane

On the machine the CLI lives on:

```bash
nvkectl config set-site --name my-tier0-site \
  --url http://<VIP>:30080 \
  --kubeconfig ./my-tier0-site.kubeconfig
nvkectl config view
```

`--url` is the controller's API. Port 30080 is the plain-HTTP NodePort the chart exposes, and the CLI sends a bearer token to it, so use it from a trusted network only, or put a TLS endpoint in front of it and name that. `--kubeconfig` is the site cluster's kubeconfig; `get
providers` and `get kubeconfig` read the cluster directly and need it.

Controller API commands (`get`/`create`/`patch`/`delete` clusters) use that
kubeconfig to authenticate: they read the audience from the authentication
ConfigMap and mint a TokenRequest for `nke-clients/nke-client`. Two names are
searched, in each of `nvke-controller`, then `nke-controller` for a site
installed before the `nvke` rename, then `cloud-workflow-engine`:
`nvke-auth-config` first, then `nke-auth-config`, which a site installed before
the `nvke` rename keeps by pinning `apiAuth.configMap.name`. The first *readable*
one is used.

Readable matters because RBAC scoped to `resourceNames` denies the names it does
not grant: a site that grants `get` on its pinned name alone answers Forbidden
for the other. The search moves past such a denial to the next candidate, so a
kubeconfig that can read only the pinned ConfigMap still mints a token.

The CLI sends no bearer token only when neither name is found in either
namespace; that is read as authentication being off. A denial is not that — it
is kept, and if no candidate turns out to be readable the command fails with it
rather than sending a bare request the controller would reject.

The kubeconfig identity needs `create` on `serviceaccounts/token` for
`nke-client`. See
[Configuring NVKE API Authentication](../../controller/docs/configuring-api-authentication.md).

### 7. Check the infrastructure providers

A tenant cluster is created on an infrastructure provider: a controller running
on the site cluster that holds credentials for its backend and creates machines
there. Once step 5 is done, confirm the site can use it:

```bash
nvkectl get providers
```

`get providers` lists each provider the CLI knows about and its state, so you
can tell an uninstalled provider from a misconfigured one before creating a
cluster on it. `CONFIGURED` means the provider's CRDs are established, its
controller is running, and its credentials are in place; it does not prove the
credentials work. Needs `kubectl` on `PATH` and the site kubeconfig from
step 6.

`CREDENTIALS` answers whether they work: it is the provider's own last
credential check, read from the `NicoIdentity` that names its credentials
Secret, and `LAST CHECK` says how long ago that check ran. Look for `CONFIGURED`
with `HEALTHY`. `HEALTHY` means the credentials authenticated recently; it does
not prove that provisioning will succeed. `NO-IDENTITY` and `UNSUPPORTED` mean
nothing is checking the credentials yet; see
[Infrastructure providers](https://github.com/dsx-ai-factory/nvke/blob/main/cli/README.md#infrastructure-providers) for what each
value means.

`nvkectl` only reads this state and never changes it, so it makes no difference
whether Helm, Argo CD, or a person applied the provider — the same command
reports it either way. On a site managed by Argo CD, that makes `get providers`
the check that what Argo synced is something a provider can actually use.

### 8. Create a tenant cluster

```bash
nvkectl create cluster -f examples/capd-cluster.yaml

# or from stdin
cat examples/capd-cluster.yaml | nvkectl create cluster -f -
```

The spec is a `CreateClusterRequest`; the schema is served at `/api/docs` on
the management plane. [examples/capd-cluster.yaml](https://github.com/dsx-ai-factory/nvke/blob/main/cli/examples/capd-cluster.yaml)
is a starting point.

### 9. Watch it come up

```bash
nvkectl get clusters
nvkectl get clusters <cluster-name>
nvkectl get clusters <cluster-name> -o yaml
```

### 10. Get the tenant cluster's kubeconfig

```bash
nvkectl get kubeconfig <cluster-name> --output-file ./tenant.kubeconfig
```

Without `--output-file`, the kubeconfig is written to stdout. This reads the
site cluster directly, the same as `get providers`, so it needs the site
kubeconfig from step 6.

### 11. Patch the tenant cluster

```bash
# Bump the Kubernetes version
nvkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'

# From a patch file
nvkectl patch cluster my-cluster --patch-file patch.yaml
```

Only the fields in the patch document are changed. `nvkectl patch cluster
--help` covers `--patch`, `--patch-file`, and field clearing in full.

### 12. Add a node pool

```bash
nvkectl create nodepool -f pool.yaml

# or from stdin
cat pool.yaml | nvkectl create nodepool -f -
```

The spec is a `CreateNodePoolRequest`; the schema is served at `/api/docs` on
the management plane. A file holding several documents is rejected; create one
pool per invocation.

`name`, `nodeNamePrefix`, `desiredNodeCount` and `clusterID` are required;
`nvkectl get clusters <name>` reports the cluster's ID. A pool carries the
same `nico` or `infra` payload as its cluster, and that is where the node type
is chosen. `nvkectl create nodepool` adds workers; the control plane is managed
through the cluster's `controlPlaneConfig`.

[examples/nico-mock-nodepool.yaml](https://github.com/dsx-ai-factory/nvke/blob/main/cli/examples/nico-mock-nodepool.yaml) is a
starting point. Provisioning is asynchronous, the same as cluster creation.

### 13. Scale a node pool

```bash
nvkectl patch nodepool <nodepool-name> --cluster <cluster-name> --patch 'desiredNodeCount: 8'

# From a file ('-' reads stdin)
nvkectl patch nodepool <nodepool-name> --cluster <cluster-name> --patch-file patch.yaml
```

Node pool names are unique within a cluster, so `--cluster` is what decides
which pool is scaled. `desiredNodeCount` is the only field this command
supports today; the API accepts more, but anything else is rejected before the
request is sent.

Scaling is asynchronous: the command returns once the controller has recorded the change,
and the management plane provisions or tears down nodes to match it. A rejected
scale-up, such as one the cluster's pod CIDR has no room for, is reported with
the controller's own message. There is no confirmation prompt. A lower count removes
only pending nodes that provisioning has not started; running nodes stay until you
delete them.


### 14. Delete a node pool

```bash
nvkectl delete nodepool <nodepool-name> --cluster <cluster-name>
```

Node pool names are unique within a cluster, so `--cluster` is what decides
which pool is resolved. Deletion is asynchronous and the nodes are drained
before removal; run `nvkectl get nodepools --cluster <cluster-name>` to check
on it. On a management plane with the control-plane controller off, deleting a
cluster's last joined control-plane node pool is rejected with the NVKE controller's own
message — delete the cluster instead.

### 15. Delete the tenant cluster

```bash
nvkectl delete cluster <cluster-name>
```
