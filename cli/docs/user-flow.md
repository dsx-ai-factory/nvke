# Site and Tenant Cluster Flow

What `nkectl` does today, in the order an operator uses it. Every command
below is on `main`; the steps the CLI does not cover are marked.
`nkectl <command> --help` is the authoritative reference for flags.

## Commands

| Command | What it does | Status |
|---|---|---|
| `nkectl generate sitespec <name> --nodes hosts.csv` | Writes a site spec for `bootstrap site` from a node CSV. `--set path=value` fills in what the CSV does not carry. | Available |
| `nkectl bootstrap site -f sitespec.yaml` | Stands up an ephemeral Kind cluster running CWE, has CWE create the site cluster on the spec's hosts through `nke-agent`, writes `<name>.kubeconfig` and `<name>-etcd-encryption-key.json`, tears the Kind cluster down. `--dry-run` validates the spec only. | Available |
| `nkectl get bootstrap-manifest` | Prints the charts and images `bootstrap site` fetches, with version and digest. | Available |
| Install an infrastructure provider on the new cluster | Install Cluster API and CAPNICo, before the site controller. See [Infrastructure Providers](../../docs/site-bootstrap/infrastructure-providers.md). No `nkectl` command. | Not in the CLI |
| Install the site controller on the new cluster | Deploy the controller Helm chart (`controller/deploy/charts/controller`). No `nkectl` command yet. | Not in the CLI |
| `nkectl config set-site --url <cwe-url> --kubeconfig <path>` | Points the CLI at a site controller. `use-site` switches between entries, `view` prints the config. | Available |
| `nkectl get providers` | Lists the Cluster API infrastructure providers the site controller can use, with their state. Alpha. | Available |
| `nkectl create cluster -f spec.yaml` | Creates a tenant cluster from a `CreateClusterRequest` YAML. `-f -` reads stdin. | Available |
| `nkectl get clusters [name ...]` | Lists tenant clusters. Names narrow to those clusters. `-o json\|yaml` changes the format. | Available |
| `nkectl get kubeconfig <name>` | Fetches a tenant cluster's admin kubeconfig. `--output-file` writes it to a file instead of stdout. | Available |
| `nkectl delete cluster <name>` | Deletes a tenant cluster. | Available |
| `nkectl patch cluster <name>` | PATCHes a mutable cluster property (Kubernetes version, etcd encryption, remote access, control plane config). `--patch` takes an inline YAML document, `--patch-file` a file (or stdin via `-`). | Available |
| `nkectl create nodepool -f spec.yaml` | Adds a node pool to an existing cluster from a `CreateNodePoolRequest` YAML. `-f -` reads stdin. | Available |
| `nkectl get nodepools [name ...]` | Lists node pools. Names narrow to those pools, `--cluster <name>` to one cluster. `-o json\|yaml` changes the format. | Available |
| `nkectl delete nodepool <name> --cluster <name>` | Removes a node pool from a cluster. A cluster has a control-plane pool only on a site controller with the control-plane controller off; deleting its last joined one is rejected, so delete the cluster instead. | Available |
| `nkectl patch nodepool <name> --cluster <name>` | PATCHes a node pool. `--patch` takes an inline YAML document, `--patch-file` a file (or stdin via `-`). `desiredNodeCount` is the only field supported so far. | Available |
| `nkectl version` | Prints version, commit and build date. `-o short\|yaml\|json`. | Available |

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
nkectl generate sitespec my-tier0-site --nodes hosts.csv > sitespec.yaml

# Override fields the CSV does not carry
nkectl generate sitespec my-tier0-site --nodes hosts.csv \
  --set cluster.controlPlaneVIP=10.10.50.10 \
  --set cluster.podCidr=10.244.0.0/16 > sitespec.yaml
```

The file format is described in [sitespec.md](sitespec.md); a complete example
is in [examples/sitespec.yaml](../examples/sitespec.yaml).

### 2. Bootstrap the site cluster

```bash
# Validate only: load, check and print the spec, touch nothing
nkectl bootstrap site -f sitespec.yaml --dry-run

# Bootstrap. The registry credential comes from the environment, not the file.
REGISTRY_API_KEY=... nkectl bootstrap site -f sitespec.yaml
```

The command needs nerdctl and containerd on the host, and network reach to
the hosts' `nke-agent` ports. On success it
writes `my-tier0-site.kubeconfig` and `my-tier0-site-etcd-encryption-key.json`
in the working directory (`--kubeconfig-out` and `--encryption-key-out` change
the paths) and removes the Kind cluster. Every control-plane node holds the
etcd encryption key; the key file is a convenience for installing a controller
later and the copy a disaster recovery needs. Move it off the host. On failure it writes cluster
diagnostics to `cluster-diagnostics/` first; `--keep-bootstrap-cluster` keeps
the Kind cluster for debugging.

The Kind cluster runs Postgres, Temporal, SPIRE, Cluster API and CWE.
`nkectl get bootstrap-manifest` lists the charts and images the bootstrap
fetches, with their versions. It leaves out the kind node image and what
`clusterctl init` installs. The `NKECTL_*` variables in the
[README](../README.md) point a chart or an image at another location.

### 3. Install an infrastructure provider

Install Cluster API and CAPNICo onto the site cluster, before the site
controller — see [Infrastructure Providers](../../docs/site-bootstrap/infrastructure-providers.md)
for why the order matters. `nkectl` has no command for this step.

### 4. Install the site controller

The site cluster from step 2 is a plain Kubernetes cluster. The site controller
(CWE) that manages tenant clusters is installed with the controller Helm chart
in [`controller/deploy/charts/controller`](../../controller/deploy/charts/controller).
`nkectl` has no command for this step yet.

### 5. Point the CLI at the site controller

```bash
nkectl config set-site --name my-tier0-site \
  --url https://cwe.my-tier0-site.example.com \
  --kubeconfig ./my-tier0-site.kubeconfig
nkectl config view
```

`--url` is the CWE API. `--kubeconfig` is the site cluster's kubeconfig; `get
providers` and `get kubeconfig` read the cluster directly and need it.

CWE API commands (`get`/`create`/`patch`/`delete` clusters) use that
kubeconfig to authenticate: they read the audience from ConfigMap
`cloud-workflow-engine/nke-auth-config` and mint a TokenRequest for
`nke-clients/nke-client`. If the ConfigMap is absent, authentication is off
and the CLI sends no bearer token.

The kubeconfig identity needs `create` on `serviceaccounts/token` for
`nke-client`. See
[Configuring NKE API Authentication](../../controller/docs/configuring-api-authentication.md).

### 6. Check the infrastructure providers

A tenant cluster is created on an infrastructure provider: a controller running
on the site cluster that holds credentials for its backend and creates machines
there. Once step 3 is done, confirm the site can use it:

```bash
nkectl get providers
```

`get providers` lists each provider the CLI knows about and its state, so you
can tell an uninstalled provider from a misconfigured one before creating a
cluster on it. `CONFIGURED` means the provider's CRDs are established, its
controller is running, and its credentials are in place; it does not prove the
credentials work. Needs `kubectl` on `PATH` and the site kubeconfig from
step 5.

`nkectl` only reads this state and never changes it, so it makes no difference
whether Helm, Argo CD, or a person applied the provider — the same command
reports it either way. On a site managed by Argo CD, that makes `get providers`
the check that what Argo synced is something a provider can actually use.

### 7. Create a tenant cluster

```bash
nkectl create cluster -f examples/capd-cluster.yaml

# or from stdin
cat examples/capd-cluster.yaml | nkectl create cluster -f -
```

The spec is a `CreateClusterRequest`; the schema is served at `/api/docs` on
the site controller. [examples/capd-cluster.yaml](../examples/capd-cluster.yaml)
is a starting point.

### 8. Watch it come up

```bash
nkectl get clusters
nkectl get clusters <cluster-name>
nkectl get clusters <cluster-name> -o yaml
```

### 9. Get the tenant cluster's kubeconfig

```bash
nkectl get kubeconfig <cluster-name> --output-file ./tenant.kubeconfig
```

Without `--output-file`, the kubeconfig is written to stdout. This reads the
site cluster directly, the same as `get providers`, so it needs the site
kubeconfig from step 5.

### 10. Patch the tenant cluster

```bash
# Bump the Kubernetes version
nkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'

# From a patch file
nkectl patch cluster my-cluster --patch-file patch.yaml
```

Only the fields in the patch document are changed. `nkectl patch cluster
--help` covers `--patch`, `--patch-file`, and field clearing in full.

### 11. Add a node pool

```bash
nkectl create nodepool -f pool.yaml

# or from stdin
cat pool.yaml | nkectl create nodepool -f -
```

The spec is a `CreateNodePoolRequest`; the schema is served at `/api/docs` on
the site controller. A file holding several documents is rejected; create one
pool per invocation.

`name`, `nodeNamePrefix`, `desiredNodeCount` and `clusterID` are required;
`nkectl get clusters <name>` reports the cluster's ID. A pool carries the
same `nico` or `infra` payload as its cluster, and that is where the node type
is chosen. `nkectl create nodepool` adds workers; the control plane is managed
through the cluster's `controlPlaneConfig`.

[examples/nico-mock-nodepool.yaml](../examples/nico-mock-nodepool.yaml) is a
starting point. Provisioning is asynchronous, the same as cluster creation.

### 12. Scale a node pool

```bash
nkectl patch nodepool <nodepool-name> --cluster <cluster-name> --patch 'desiredNodeCount: 8'

# From a file ('-' reads stdin)
nkectl patch nodepool <nodepool-name> --cluster <cluster-name> --patch-file patch.yaml
```

Node pool names are unique within a cluster, so `--cluster` is what decides
which pool is scaled. `desiredNodeCount` is the only field this command
supports today; the API accepts more, but anything else is rejected before the
request is sent.

Scaling is asynchronous: the command returns once CWE has recorded the change,
and the site controller provisions or tears down nodes to match it. A rejected
scale-up, such as one the cluster's pod CIDR has no room for, is reported with
CWE's own message. There is no confirmation prompt, so a lower count starts
removing nodes straight away.


### 13. Delete a node pool

```bash
nkectl delete nodepool <nodepool-name> --cluster <cluster-name>
```

Node pool names are unique within a cluster, so `--cluster` is what decides
which pool is resolved. Deletion is asynchronous and the nodes are drained
before removal; run `nkectl get nodepools --cluster <cluster-name>` to check
on it. On a site controller with the control-plane controller off, deleting a
cluster's last joined control-plane node pool is rejected with CWE's own
message — delete the cluster instead.

### 14. Delete the tenant cluster

```bash
nkectl delete cluster <cluster-name>
```
