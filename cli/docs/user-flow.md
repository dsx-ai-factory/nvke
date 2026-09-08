# Site and Tenant Cluster Flow

What `nkectl` does today, in the order an operator uses it. Every command
below is on `main`; the steps the CLI does not cover yet are marked.
`nkectl <command> --help` is the authoritative reference for flags.

## Commands

| Command | What it does | Status |
|---|---|---|
| `nkectl generate sitespec <name> --nodes hosts.csv` | Writes a site spec for `bootstrap site` from a node CSV. `--set path=value` fills in what the CSV does not carry. | Available |
| `nkectl bootstrap site -f sitespec.yaml` | Stands up an ephemeral Kind cluster running CWE, has CWE create the site cluster on the spec's hosts through `nke-agent`, writes `<name>.kubeconfig`, tears the Kind cluster down. `--dry-run` validates the spec only. | Available |
| Install the site controller on the new cluster | Deploy the controller Helm chart (`controller/deploy/charts/controller`). No `nkectl` command yet. | Not in the CLI |
| `nkectl config set-site --url <cwe-url> --kubeconfig <path>` | Points the CLI at a site controller. `use-site` switches between entries, `view` prints the config. | Available |
| `nkectl configure provider --type <nico\|launchlayer> ...` | Gives an installed infrastructure provider the endpoint and credentials it needs to create machines. Alpha. | Planned |
| `nkectl get providers` | Lists the Cluster API infrastructure providers the site controller can use, with their state. Alpha. | Available |
| `nkectl create cluster -f spec.yaml` | Creates a tenant cluster from a `CreateClusterRequest` YAML. `-f -` reads stdin. | Available |
| `nkectl get clusters` | Lists tenant clusters. `-c <name>` filters, `-o json\|yaml` changes the format. | Available |
| `nkectl describe cluster <name>` | Shows one tenant cluster. `-o json\|yaml` changes the format. | Available |
| `nkectl get kubeconfig <name>` | Fetches a tenant cluster's admin kubeconfig. | Not on `main`: [#269](https://github.com/NVIDIA/nke/pull/269) |
| `nkectl delete cluster <name>` | Deletes a tenant cluster. | Available |
| `nkectl version` | Prints version, commit and build date. `-o short\|yaml\|json`. | Available |

`cluster` and `clusters` are aliases on `create`, `describe`, `get` and
`delete`. Global flags: `--site <name>` overrides the active site for one
invocation, `-v` enables debug logging, `--json-logs` emits logs as JSON.

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
  --set cluster.cniName=calico \
  --set cluster.podCidr=10.100.0.0/16 > sitespec.yaml
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

The command needs Docker or nerdctl on the host (`--bootstrap-provider`, nerdctl
by default) and network reach to the hosts' `nke-agent` ports. On success it
writes `my-tier0-site.kubeconfig` in the working directory (`--kubeconfig-out`
changes the path) and removes the Kind cluster. On failure it writes cluster
diagnostics to `cluster-diagnostics/` first; `--keep-bootstrap-cluster` keeps
the Kind cluster for debugging.

### 3. Install the site controller

The site cluster from step 2 is a plain Kubernetes cluster. The site controller
(CWE) that manages tenant clusters is installed with the controller Helm chart
in [`controller/deploy/charts/controller`](../../controller/deploy/charts/controller).
`nkectl` has no command for this step yet.

### 4. Point the CLI at the site controller

```bash
nkectl config set-site --name my-tier0-site \
  --url https://cwe.my-tier0-site.example.com \
  --kubeconfig ./my-tier0-site.kubeconfig
nkectl config view
```

`--url` is the CWE API. `--kubeconfig` is the site cluster's kubeconfig; `get
providers` and, once it lands, `get kubeconfig` read the cluster directly and
need it.

### 5. Configure and check the infrastructure providers

A tenant cluster is created on an infrastructure provider that is installed on
the site cluster and holds credentials for its backend. Configure one, then
confirm the site can use it:

```bash
# Planned: gives the provider its backend endpoint and credentials
nkectl configure provider --type nico ...

nkectl get providers
```

`get providers` lists each provider the CLI knows about and its state;
`CONFIGURED` means the provider is installed and has credentials. Needs
`kubectl` on `PATH`.

On a site where Argo CD manages the providers, do not run `configure provider`.
Keep the provider and its credentials in git and let Argo apply them, as for the
rest of the site. `get providers` is the same either way: it reports what is on
the cluster, whichever tool put it there.

### 6. Create a tenant cluster

```bash
nkectl create cluster -f examples/capd-cluster.yaml

# or from stdin
cat examples/capd-cluster.yaml | nkectl create cluster -f -
```

The spec is a `CreateClusterRequest`; the schema is served at `/api/docs` on
the site controller. [examples/capd-cluster.yaml](../examples/capd-cluster.yaml)
is a starting point.

### 7. Watch it come up

```bash
nkectl get clusters
nkectl describe cluster <cluster-name>
nkectl describe cluster <cluster-name> -o yaml
```

### 8. Get the tenant cluster's kubeconfig

`nkectl get kubeconfig <cluster-name>` is in review
([#269](https://github.com/NVIDIA/nke/pull/269)). Until it merges, CWE
publishes the kubeconfig as a Secret in the site cluster:

```bash
kubectl --kubeconfig ./my-tier0-site.kubeconfig -n nke-system \
  get secret <cluster-name>-admin-kubeconfig -o jsonpath='{.data.value}' | base64 -d
```

### 9. Delete the tenant cluster

```bash
nkectl delete cluster <cluster-name>
```
