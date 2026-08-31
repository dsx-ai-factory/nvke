# Site and Tenant Cluster Flow

## Commands & Status

| Command | Owner(s) | Status |
|---|---|---|
| `nkectl generate inventory` | Unassigned | Planning |
| `nkectl bootstrap site` | Lia Campbell, @jonathanmeier5 | Implementation |
| `nkectl site deploy` | Unassigned | Planning |
| `nkectl create cluster` | @anisha-nv | Complete |
| `nkectl list clusters` | @anisha-nv | Complete |
| `nkectl get kubeconfig` | @anisha-nv | Implementation |
| `nkectl delete clusters` | @anisha-nv | Implementation |
| TBD: generate & install aicr charts on tenant cluster  | TBD | TBD |

Support for managing the site cluster itself is still in planning. @jonathanmeier5 is working with @nhammernik and @vivin on this.


## Example

### 1. Generate inventory file

```bash
nkectl generate inventory --name my-tier0-site > inventory.yaml
```

### 2. Bootstrap cluster

```bash
nkectl bootstrap site -i inventory.yaml
```

### 3. Deploy CWE management plane to cluster
```bash
nkectl site deploy
```

### 4. Create a tenant cluster

```bash
nkectl create cluster examples/capd-cluster.yaml
```

### 5. List the clusters

```bash
nkectl list clusters
```

### 6. Get the tenant cluster kubeconfig

```bash
nkectl get kubeconfig <cluster-name>
```

### 7. Delete a tenant cluster

```bash
nkectl delete clusters <cluster-name>
```
