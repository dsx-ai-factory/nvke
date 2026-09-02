# Deploy Repository

> **Note:** The deploy repository is a **post-bootstrap concern**. `nkectl bootstrap site` brings up the Kubernetes cluster; deploying NKE's services (CWE, Temporal, CNPG) onto that cluster via ArgoCD is a separate subsequent step. This page describes that second step.

A deploy repository contains the ArgoCD Application manifests, secrets, and Kubernetes manifests that deploy the NKE services stack onto a freshly bootstrapped site controller cluster.

## Reference Repository

A complete, ready-to-fork reference repository is available at `github.com/NVIDIA/nke` under `deploy/reference/`. It includes the `nke-base-config` templates for secrets and namespaces, storage manifests, and ArgoCD Application manifests for all required NKE components.

> **Important:** The deploy repository must be accessible from the machine running the post-bootstrap deployment tooling. Ensure the repository is hosted on a git service reachable from your provisioned nodes (public GitHub, GitLab.com, or another externally accessible host).

To use the reference repo:

1. Fork or copy it into your own git repository
2. Edit subdirectories under `deploy/` for your environment (chart versions, image tags, storage classes, database sizing)
3. Push to your git remote
4. Point the post-bootstrap deployment tooling at your fork:

```yaml
argocd:
  repos:
    - url: "https://github.com/my-org/my-nke-deploy.git"
      token: "ghp-xxxxxxxxxxxx"
      deployPaths:
        - "deploy"
```

## Repository Layout

A deploy repository has one or more **deploy paths** — directories that the post-bootstrap deployment tooling processes. Each deploy path contains subdirectories, one per deploy item.

```text
my-deploy-repo/
├── deploy/                              # <-- deployPath
│   ├── nke-deploy.yaml                  # declares item order and types
│   ├── nke-base-config/                 # template: namespaces, secrets
│   │   ├── 00-namespaces.yaml
│   │   ├── 10-image-pull-secret.yaml.tmpl
│   │   ├── 20-argocd-repo-secrets.yaml.tmpl
│   │   ├── 30-cnpg-secrets.yaml.tmpl
│   │   ├── 40-forge-api-token.yaml.tmpl
│   │   └── 50-monitoring-secrets.yaml.tmpl
│   ├── nke-storage/                     # manifest: StorageClasses and PVs
│   │   ├── storage-classes.yaml
│   │   └── persistent-volumes.yaml
│   ├── kubernetes-replicator/           # argocd: replicates pull secrets
│   │   └── application.yaml
│   ├── cloudnative-pg/                  # argocd: CNPG operator
│   │   └── application.yaml
│   ├── cnpg-temporal/                   # argocd: PostgreSQL for Temporal
│   │   └── application.yaml
│   └── ...
```

Flat files at the deploy path root (other than `nke-deploy.yaml`) are ignored — only subdirectories are processed.

## The `nke-deploy.yaml` File

Place an `nke-deploy.yaml` at the root of each deploy path to declare item order and types explicitly:

```yaml
items:
  - name: nke-base-config
    type: template
  - name: nke-storage
    type: manifest
  - name: kubernetes-replicator
    type: argocd
  - name: cloudnative-pg
    type: argocd
  - name: cnpg-temporal
    type: argocd
  - name: temporal
    type: argocd
  - name: cloud-workflow-engine
    type: argocd
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Subdirectory name. For `argocd` items, also becomes the ArgoCD Application name. |
| `type` | Yes | `template`, `manifest`, or `argocd` |
| `path` | No | Subdirectory path relative to deploy root. Defaults to `name`. |

Names must be unique across all repos — duplicates are skipped with a warning.

If `nke-deploy.yaml` is absent, the post-bootstrap deployment tooling falls back to **auto-detection**: subdirectories are processed in sorted order; those containing `application.yaml` are treated as ArgoCD apps, everything else as plain manifests.

## Deploy Item Types

### `template` — Go Template Rendering

Template items contain `.yaml.tmpl` files (and optionally plain `.yaml` files). Before the deploy repo is uploaded to the init node, the post-bootstrap deployment tooling renders each `.yaml.tmpl` file using Go's `text/template` engine with the full [`BootstrapConfig`](#bootstrapconfig-variable-reference) as template data. Rendered output replaces the `.tmpl` files in-place, and the item is then applied as a manifest.

This is how secrets and cluster-specific configuration that depend on `BootstrapConfig` values (NGC keys, database passwords, repo tokens, etc.) are created without hardcoding them in the repository.

Files within a template item are applied in lexicographic order. Use numeric prefixes (e.g. `00-`, `10-`, `20-`) to control ordering.

### `manifest` — Plain Kubernetes Manifests

Manifest items contain `.yaml` and `.yml` files applied directly via `kubectl apply --server-side`. Files are applied in lexicographic order.

### `argocd` — ArgoCD Applications

ArgoCD items must contain an `application.yaml` file with a valid `argoproj.io/v1alpha1` Application manifest. The application is created via `argocd app create -f &lt;path&gt; --upsert`.

| Field | Requirement |
|-------|-------------|
| `metadata.name` | Must match the parent directory name |
| `metadata.namespace` | Must be `argocd` |
| `spec.source(s)[].repoURL` | Must match a URL that has a corresponding ArgoCD repo secret |
| `spec.destination.server` | Typically `https://kubernetes.default.svc` |

Single-source example (inline Helm values):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cloud-workflow-engine
  namespace: argocd
spec:
  project: default
  destination:
    namespace: cloud-workflow-engine
    server: https://kubernetes.default.svc
  source:
    repoURL: https://helm.ngc.nvidia.com/j7sbcjl3qgta
    chart: cloud-workflow-engine
    targetRevision: "0.1.0"
    helm:
      values: |
        app:
          image: nvcr.io/j7sbcjl3qgta/cloud-workflow-engine:v0.1.0
  syncPolicy:
    automated: {}
    syncOptions:
      - CreateNamespace=true
```

Multi-source example (separate values file via `$values` ref):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: aoa-infra
  namespace: argocd
spec:
  project: default
  destination:
    namespace: argocd
    server: https://kubernetes.default.svc
  sources:
    - repoURL: https://github.com/my-org/my-deploy-repo.git
      targetRevision: main
      path: aoa-infra/v1
      helm:
        valueFiles:
          - $values/environments/prod/my-site/aoa-infra/values.yaml
    - repoURL: https://github.com/my-org/my-deploy-repo.git
      targetRevision: main
      ref: values
  syncPolicy:
    automated: {}
    syncOptions:
      - CreateNamespace=true
```

## How Deploy Processing Works

```mermaid
flowchart TD
    A["Go binary: clone repo"] --> B["Parse nke-deploy.yaml"]
    B --> C{"Has template items?"}
    C -- Yes --> D["Render .yaml.tmpl files"]
    D --> E["Replace .tmpl with .yaml in-place"]
    E --> F["Archive and upload to init node"]
    C -- No --> F
    F --> G["Shell script: process items in order"]
    G --> H{"Item type?"}
    H -- "template/manifest" --> I["kubectl apply --server-side"]
    H -- "argocd" --> J["argocd app create --upsert"]
    I --> K["Next item"]
    J --> K
    K --> L["Wait for ArgoCD apps to sync"]
```

For each repo with `deployPaths`, the post-bootstrap deployment tooling:

1. Clones the repository locally (shallow, `--depth 1 --single-branch`)
2. Parses `nke-deploy.yaml` in each deploy path (if present)
3. Renders `type: template` items using the `BootstrapConfig`
4. Archives the processed repo and uploads it to the init node
5. The shell script processes items in declared order
6. Waits for ArgoCD applications to sync, then waits for required child apps

## BootstrapConfig Variable Reference

Template files use Go [`text/template`](https://pkg.go.dev/text/template) syntax. The template context (`.`) is the full `BootstrapConfig` struct.

### Top-Level Fields

| Expression | Type | Description |
|------------|------|-------------|
| `.ClusterName` | string | Cluster name |
| `.ForgeAPIKey` | string | Infrastructure provider API key |
| `.OrgID` | string | Organization ID |
| `.BaremetalMode` | bool | Whether baremetal mode is active |

### `.Cluster`

| Expression | Type | Description |
|------------|------|-------------|
| `.Cluster.Nodes` | list | Control-plane nodes (iterate with `range`) |
| `.Cluster.K8sVersion` | string | Kubernetes version |
| `.Cluster.CNIName` | string | CNI name (`calico`, `ovn-kubernetes`) |
| `.Cluster.PodCIDR` | string | Pod CIDR |

Each node in `.Cluster.Nodes`:

| Expression | Type | Description |
|------------|------|-------------|
| `.Name` | string | Node hostname |
| `.IP` | string | Node IP address |

### `.Registry`

| Expression | Type | Description |
|------------|------|-------------|
| `.Registry.NGCAPIKey` | string | NGC API key |

### `.ArgoCD`

| Expression | Type | Description |
|------------|------|-------------|
| `.ArgoCD.Version` | string | ArgoCD version |
| `.ArgoCD.Repos` | list | Configured repositories |

Each repo in `.ArgoCD.Repos`:

| Expression | Type | Description |
|------------|------|-------------|
| `.URL` | string | Repository URL |
| `.Token` | string | Access token |
| `.Username` | string | Git username for ArgoCD repo secrets |
| `.Branch` | string | Branch name |
| `.DeployPaths` | list of strings | Deploy path directories |

### `.Database`

| Expression | Type | Description |
|------------|------|-------------|
| `.Database.SuperuserPassword` | string | CNPG superuser password |
| `.Database.CloudPassword` | string | Cloud DB application password |
| `.Database.SitePassword` | string | Site DB application password |
| `.Database.TemporalPassword` | string | Temporal DB password |

### `.Monitoring`

| Expression | Type | Description |
|------------|------|-------------|
| `.Monitoring.PrometheusUsername` | string | Prometheus remote-write username |
| `.Monitoring.PrometheusPassword` | string | Prometheus remote-write password |
| `.Monitoring.LokiUsername` | string | Loki push username |
| `.Monitoring.LokiPassword` | string | Loki push password |
| `.Monitoring.LokiHost` | string | Loki push endpoint |

### `.KubeVip`

| Expression | Type | Description |
|------------|------|-------------|
| `.KubeVip.Enabled` | bool | Whether kube-vip is enabled |
| `.KubeVip.VIP` | string | Virtual IP address |
| `.KubeVip.Version` | string | kube-vip version |
| `.KubeVip.Interface` | string | Network interface |
| `.KubeVip.VIPRanges` | string | Comma-separated VIP CIDR ranges |

## Template Examples

### Conditional secret

Only rendered when monitoring credentials are configured:

```yaml
{{- if and .Monitoring.PrometheusUsername .Monitoring.PrometheusPassword }}
apiVersion: v1
kind: Secret
metadata:
  name: prometheus-auth
  namespace: monitoring
stringData:
  username: "{{ .Monitoring.PrometheusUsername }}"
  password: "{{ .Monitoring.PrometheusPassword }}"
{{- end }}
```

### Iterating over repos

Creates an ArgoCD repo secret for each configured repository:

```yaml
{{- range $i, $repo := .ArgoCD.Repos }}
---
apiVersion: v1
kind: Secret
metadata:
  name: repo-aoa-{{ $i }}
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  type: git
  url: "{{ $repo.URL }}"
  username: "{{ $repo.Username }}"
  password: "{{ $repo.Token }}"
{{- end }}
```

### Conditional on baremetal mode

```yaml
{{- if not .BaremetalMode }}
apiVersion: v1
kind: Secret
metadata:
  name: forge-creds
  namespace: cloud-workflow-engine
stringData:
  forge-api-key: "{{ .ForgeAPIKey }}"
{{- end }}
```

### Multi-document YAML

A single `.yaml.tmpl` file can produce multiple Kubernetes resources:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: superuser-secret
  namespace: cnpg-cloud
type: kubernetes.io/basic-auth
stringData:
  username: postgres
  password: "{{ .Database.SuperuserPassword }}"
---
apiVersion: v1
kind: Secret
metadata:
  name: superuser-secret
  namespace: cnpg-site
type: kubernetes.io/basic-auth
stringData:
  username: postgres
  password: "{{ .Database.SuperuserPassword }}"
```

## Built-in `nke-base-config` Templates

The reference repository includes an `nke-base-config` template item that creates all the base resources needed before ArgoCD applications are deployed. It must be the first item in the deploy sequence.

| File | Resources Created |
|------|-------------------|
| `00-namespaces.yaml` | Namespaces: `kubernetes-replicator`, `cnpg-cloud`, `cnpg-site`, `cnpg-temporal`, `cloud-workflow-engine`, `monitoring` |
| `10-image-pull-secret.yaml.tmpl` | `ngc-registry-nke-imagepull` image pull secret |
| `20-argocd-repo-secrets.yaml.tmpl` | ArgoCD repo secrets for each configured repo, plus NGC Helm repo secrets |
| `30-cnpg-secrets.yaml.tmpl` | CNPG superuser and per-app bootstrap secrets across 3 namespaces |
| `40-forge-api-token.yaml.tmpl` | Infrastructure provider API token (skipped in baremetal mode) |
| `50-monitoring-secrets.yaml.tmpl` | Grafana/Prometheus/Loki credentials (conditional) |

## Secret-Only Repos

Repos that omit `deployPaths` are "secret-only." the post-bootstrap deployment tooling does not clone them; they exist solely so that ArgoCD can fetch from them at runtime.

> **Important:** the post-bootstrap deployment tooling does **not** automatically create ArgoCD repo secrets. Your deploy repository must include a template that creates them. The reference repo's `20-argocd-repo-secrets.yaml.tmpl` handles this by iterating over `.ArgoCD.Repos`. If you build a bespoke deploy repository without `nke-base-config`, you must create these secrets yourself or ArgoCD will fail to fetch from your repos.

```yaml
argocd:
  repos:
    - url: "https://github.com/my-org/deploy.git"
      token: "ghp-xxx"
      deployPaths:
        - "deploy"
    - url: "https://github.com/my-org/helm-charts.git"
      token: "ghp-yyy"
      # Secret-only: not cloned, but needs an ArgoCD repo secret
```

## Sync and Health Monitoring

After all deploy items are processed, the post-bootstrap deployment tooling monitors ArgoCD through three phases:

1. **App-of-apps sync** — waits up to 10 minutes for each deployed application to reach `Synced` + `Healthy`
2. **Child app sync** — waits up to 60 minutes for all apps listed in `argocd.requiredApps` to reach `Synced` + `Healthy`
3. **Retry logic** — automatically retriggers sync for stuck, OutOfSync, or failed apps on a 60–120s cadence

## Adding a Deploy Item

1. Create a new directory under your deploy path (e.g., `deploy/my-app`)
2. Add the appropriate files (see item types above)
3. Add an entry to `nke-deploy.yaml` at the desired position:

```yaml
items:
  - name: nke-base-config
    type: template
  - name: my-app              # <-- new item
    type: manifest
  - name: nke-storage
    type: manifest
  # ...
```

4. Push to your repo — the post-bootstrap deployment tooling deploys items in the declared order
