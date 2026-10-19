# Site and Tenant Cluster Flow

What `nvkectl` does today, in the order an operator uses it, with the one
step that is not a CLI command marked: installing the controller stack with
the deploy script.
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
| `nvkectl get bootstrap-manifest` | Prints the charts and images `bootstrap site` fetches, with version and digest. | Available |
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

### 1. Describe the hosts and generate the site spec

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

### 2. Bootstrap the site cluster

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

### 3. Install the controller stack

Run these from a shell that has sourced the workspace's `env.sh`, as
[Run NVKE end to end on one machine](../../docs/get-started/local-site.md)
describes. It exports `NVKE_KUBECONFIG` and the install's other inputs.

```bash
clusterctl init --kubeconfig "$NVKE_KUBECONFIG" --core cluster-api:<v> --bootstrap kubeadm:<v> --control-plane kubeadm:<v> --wait-providers
"$NVKE_REPO"/deploy/nvke-deploy-local.sh install
kubectl --kubeconfig "$NVKE_KUBECONFIG" apply -f nvke-api-clients.yaml
```

A list of commands, in this order: `clusterctl init` installs Cluster API at
the pinned version; the wrapper installs the lab StorageClass, reads the
nodes' architecture from the cluster, renders and applies the `deploy/nico`
deploy path item by item, and patches the Cluster API controllers for SPIRE;
the NetworkPolicy admits your client
networks to the controller API, which the chart otherwise answers from
in-cluster pods only. Then point the CLI at the site (step 4) and publish a `MachineImage`, `ClusterRelease` and `NodePoolRelease`
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

### 4. Point the CLI at the management plane

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
kubeconfig to authenticate: they read the audience from ConfigMap
`nke-auth-config` in `nke-controller`, or in `cloud-workflow-engine` when that
is absent, and mint a TokenRequest for
`nke-clients/nke-client`. If neither holds the ConfigMap, authentication is off
and the CLI sends no bearer token.

The kubeconfig identity needs `create` on `serviceaccounts/token` for
`nke-client`. See
[Configuring NVKE API Authentication](../../controller/docs/configuring-api-authentication.md).

### 5. Check the infrastructure providers

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

### 6. Create a tenant cluster

```bash
nvkectl create cluster -f examples/capd-cluster.yaml

# or from stdin
cat examples/capd-cluster.yaml | nvkectl create cluster -f -
```

The spec is a `CreateClusterRequest`; the schema is served at `/api/docs` on
the management plane. [examples/capd-cluster.yaml](https://github.com/dsx-ai-factory/nvke/blob/main/cli/examples/capd-cluster.yaml)
is a starting point.

### 7. Watch it come up

```bash
nvkectl get clusters
nvkectl get clusters <cluster-name>
nvkectl get clusters <cluster-name> -o yaml
```

### 8. Get the tenant cluster's kubeconfig

```bash
nvkectl get kubeconfig <cluster-name> --output-file ./tenant.kubeconfig
```

Without `--output-file`, the kubeconfig is written to stdout. This reads the
site cluster directly, the same as `get providers`, so it needs the site
kubeconfig from step 6.

### 9. Patch the tenant cluster

```bash
# Bump the Kubernetes version
nvkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'

# From a patch file
nvkectl patch cluster my-cluster --patch-file patch.yaml
```

Only the fields in the patch document are changed. `nvkectl patch cluster
--help` covers `--patch`, `--patch-file`, and field clearing in full.

### 10. Add a node pool

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

### 11. Scale a node pool

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


### 12. Delete a node pool

```bash
nvkectl delete nodepool <nodepool-name> --cluster <cluster-name>
```

Node pool names are unique within a cluster, so `--cluster` is what decides
which pool is resolved. Deletion is asynchronous and the nodes are drained
before removal; run `nvkectl get nodepools --cluster <cluster-name>` to check
on it. On a management plane with the control-plane controller off, deleting a
cluster's last joined control-plane node pool is rejected with the NVKE controller's own
message — delete the cluster instead.

### 13. Delete the tenant cluster

```bash
nvkectl delete cluster <cluster-name>
```
