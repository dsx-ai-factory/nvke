# Configuration

This page documents the `tier0-bootstrap` configuration file format and ArgoCD Application repository structure.

## Before You Begin

Before running `tier0-bootstrap`, you need:

1. **A deploy repository** containing ArgoCD Application manifests and/or plain Kubernetes manifests for the [required NKE applications](#required-applications). Fork the [reference repository](#reference-repository) for a ready-to-use starting point.
2. **Credentials**: NGC API key and a git access token for your deploy repository. A Forge or NICo API key is required unless running in `baremetalMode`.

## Config File

`tier0-bootstrap` loads configuration from a YAML file, environment variables, and CLI flags. Precedence (highest to lowest):

1. **CLI flags**
2. **Environment variables**
3. **YAML config file**

Specify the config file with `--config`:

```bash
tier0-bootstrap run --config /path/to/tier0-bootstrap.yaml
```

## Example Configuration

```yaml
clusterName: "my-site-controller"

cluster:
  k8sVersion: "1.34.2"
  cniName: "calico"
  podCidr: "192.168.0.0/16"
  # initNode name and ip are auto-detected if omitted
  # joinNodes:
  #   - name: "ctrl-2"
  #     ip: "10.0.0.11"
  #   - name: "ctrl-3"
  #     ip: "10.0.0.12"

registry:
  ngcApiKey: ""  # Set via NGC_API_KEY env var

argocd:
  repos:
    - url: "https://gitlab.example.com/my-org/my-nke-deploy.git"
      token: ""  # Set via ARGOCD_REPOS env var or --argocd-repo CLI flag
      username: "oauth2"
      deployPaths:
        - "deploy"

database:
  superuserPassword: "strong-password"
  cloudPassword: "strong-password"
  sitePassword: "strong-password"
  temporalPassword: "strong-password"
```

## Keyword Reference

| Keyword | Description |
|---------|-------------|
| [`clusterName`](#clustername) | Cluster name |
| [`cluster`](#cluster) | Kubernetes cluster settings |
| [`registry`](#registry) | Container registry and image settings |
| [`argocd`](#argocd) | ArgoCD and deploy repository settings |
| [`database`](#database) | CNPG database credentials |
| [`monitoring`](#monitoring) | Observability credentials |
| [`kubevip`](#kubevip) | High availability configuration |
| [`forgeApiKey`](#forgeapikey) | Infrastructure provider API authentication |
| [`baremetalMode`](#baremetalmode) | Skip infrastructure provider API calls |
| [`enableMtls`](#enablemtls) | mTLS feature flag (deprecated) |

---

## `clusterName`

<kbd>CLUSTER_NAME</kbd>

Name of the tier-0 Kubernetes cluster.

**Type:** string (required)  
**Maximum length:** 63 characters

```yaml
clusterName: "my-site-controller"
```

---

## `cluster`

Kubernetes cluster settings including the init node, join nodes, and network parameters.

### `cluster.initNode`

<kbd>INIT_NODE_NAME</kbd> <kbd>INIT_NODE_IP</kbd>

The initial control plane node. Leave blank for auto-detection from hostname and primary network interface.

| Sub-key | Type | Default | Description |
|---------|------|---------|-------------|
| `name` | string | Auto-detected from hostname | Node hostname |
| `ip` | string | Auto-detected from primary interface | Node IP address |

```yaml
cluster:
  initNode:
    name: "ctrl-1"
    ip: "10.0.0.10"
```

### `cluster.joinNodes`

<kbd>JOIN_NODES</kbd> format: `name:ip,name:ip`

Additional control plane nodes for HA deployments. Omit for single-node clusters.

```yaml
cluster:
  joinNodes:
    - name: "ctrl-2"
      ip: "10.0.0.11"
    - name: "ctrl-3"
      ip: "10.0.0.12"
```

### `cluster.k8sVersion`

<kbd>CLUSTER_K8S_VERSION</kbd>

**Type:** string  
**Default:** `1.31.9`

### `cluster.cniName`

<kbd>CLUSTER_CNI_NAME</kbd>

**Type:** string  
**Default:** `calico`  
**Supported values:** `calico`, `ovn-kubernetes`

### `cluster.podCidr`

<kbd>CLUSTER_POD_CIDR</kbd>

**Type:** CIDR string  
**Default:** `10.244.0.0/16`

---

## `registry`

NGC container registry access.

### `registry.ngcApiKey`

<kbd>NGC_API_KEY</kbd>

NGC API key for pulling container images. **Required.**

Prefer setting this via the `NGC_API_KEY` environment variable rather than committing it to a config file.

```yaml
registry:
  ngcApiKey: "nvapi-xxxxxxxxxxxx"
```

---

## `argocd`

ArgoCD deployment settings and git repositories. This is the primary configuration surface for controlling which applications `tier0-bootstrap` deploys.

### `argocd.version`

<kbd>ARGOCD_VERSION</kbd>

**Type:** string  
**Default:** `v2.14.1`

### `argocd.autoSync`

<kbd>ARGOCD_AUTO_SYNC</kbd>

**Type:** boolean  
**Default:** `true`

### `argocd.requiredApps`

<kbd>ARGOCD_REQUIRED_APPS</kbd>

ArgoCD child applications that must reach `Synced` + `Healthy` for bootstrap to succeed. Applications not in this list are deployed but are non-blocking.

**Type:** comma-separated string  
**Default:** `cloud-workflow-engine`

```yaml
argocd:
  requiredApps: "cloud-workflow-engine"
```

### `argocd.repos`

<kbd>ARGOCD_REPOS</kbd>

Git repositories containing your deploy directories. At least one repository with `deployPaths` is required.

```yaml
argocd:
  repos:
    - url: "https://gitlab.example.com/my-org/site-deploy.git"
      token: "glpat-xxxxxxxxxxxx"
      username: "oauth2"
      deployPaths:
        - "deploy"
    - url: "https://gitlab.example.com/my-org/helm-charts.git"
      token: "glpat-yyyyyyyyyyyy"
      # Secret-only: no deployPaths
```

**CLI equivalent:**

```bash
--argocd-repo "url=https://gitlab.example.com/my-org/site-deploy.git,token=glpat-xxx,path=deploy"
```

Multiple `--argocd-repo` flags can be specified.

#### `ARGOCD_REPOS` Format

```bash
export ARGOCD_REPOS="url=https://example.com/repo.git,token=glpat-xxx,path=deploy;url=https://example.com/charts.git,token=glpat-yyy"
```

- Repos separated by `;`
- Fields within a repo separated by `,`
- Multiple deploy paths: `paths=dir1+dir2`
- Omit `path`/`paths` for secret-only repos

#### Repo Object Keywords

| Key | Required | Description |
|-----|----------|-------------|
| `url` | Yes | HTTPS git clone URL |
| `token` | Yes | Git access token (read access) |
| `deployPaths` | No | Directories within the repo to process. Omit for secret-only repos. |
| `branch` | No | Branch name. Default: `main` |
| `username` | No | Username for ArgoCD repo secret. Default: `tier0-bootstrap-user` |

For `username` by git provider:

| Provider | Value |
|----------|-------|
| GitLab (personal/group access tokens) | `oauth2` |
| GitLab (project access tokens) | Any non-empty value |
| GitHub (fine-grained PATs) | `x-access-token` |

#### How Deploy Discovery Works

For each repo with `deployPaths`, `tier0-bootstrap`:

1. Clones the repository (shallow, `--depth 1 --single-branch`)
2. Looks for `nke-deploy.yaml` in each deploy path
3. If found, processes items in declared order with explicit types
4. If absent, falls back to auto-detection: subdirectories sorted by name; `application.yaml` present → ArgoCD app, otherwise → plain manifests
5. Waits for ArgoCD applications to sync, then waits for required child applications

#### The `nke-deploy.yaml` File

```yaml
# deploy/nke-deploy.yaml
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
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Subdirectory name. For `argocd` items, also becomes the ArgoCD Application name. |
| `type` | Yes | `argocd`, `manifest`, or `template` |
| `path` | No | Subdirectory path relative to deploy root. Defaults to `name`. |

Names must be unique across all repos — duplicates are skipped with a warning.

#### Secret-Only Repos

Repos that omit `deployPaths` are "secret-only." `tier0-bootstrap` does not clone them; they exist so ArgoCD can fetch from them at runtime (e.g., a Helm chart repo referenced by your Application manifests).

---

## Required Applications

An NKE site controller cluster requires the following ArgoCD Applications:

| Application | NGC Chart | Sync Wave | Description |
|-------------|-----------|-----------|-------------|
| `cnpg-temporal` | `cnpg-cluster` | 40 | PostgreSQL for Temporal |
| `cnpg-cloud` | `cnpg-cluster` | 40 | PostgreSQL for cloud-workflow-engine |
| `temporal` | `temporal` | 50 | Temporal workflow orchestration |
| `cloud-workflow-engine` | `cloud-workflow-engine` | 60 | Cloud Workflow Engine |

> **TODO:** Confirm `j7sbcjl3qgta` is the final public NGC org for NKE Helm charts and container images. Update all `helm.ngc.nvidia.com/j7sbcjl3qgta` and `nvcr.io/j7sbcjl3qgta` references if the org changes.

All charts are published to `https://helm.ngc.nvidia.com/j7sbcjl3qgta`. To pull them, you need an NGC API key with access to the NKE organization (`j7sbcjl3qgta`). Create one at [NGC](https://ngc.nvidia.com) under the `j7sbcjl3qgta` org (**Setup** > **API Key**).

Sync waves control deployment order: databases (wave 40) must be healthy before Temporal (wave 50), which must be healthy before the workflow engines (wave 60).

### The `application.yaml` File

Each `application.yaml` must be a valid `argoproj.io/v1alpha1` Application manifest.

| Field | Requirement |
|-------|-------------|
| `metadata.name` | Must match the parent directory name |
| `metadata.namespace` | Must be `argocd` |
| `spec.source.repoURL` | Must match a URL that has a corresponding ArgoCD repo secret |
| `spec.destination.server` | Typically `https://kubernetes.default.svc` |

Example (`cloud-workflow-engine/application.yaml`):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cloud-workflow-engine
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "60"
  finalizers:
    - resources-finalizer.argocd.argoproj.io
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
          imagePullSecret: ngc-registry-nke-imagepull
        appDb:
          url: cnpg-cloud-rw.cnpg-cloud.svc.cluster.local:5432
          postgresPassword: <cloud-rw-password>
        temporal:
          url: temporal-frontend.temporal.svc.cluster.local:7233
  syncPolicy:
    automated: {}
    syncOptions:
      - CreateNamespace=true
```

### Reference Repository

> **TODO:** Create `deploy/reference/` in this repo with the reference Application manifests during code migration.

A complete reference repository with all required Application manifests is available at `github.com/NVIDIA/nke` under `deploy/reference/`. Each application is a standalone ArgoCD Application with inline Helm values.

```text
deploy/reference/
└── deploy/
    ├── cloudnative-pg/
    │   └── application.yaml
    ├── cloud-workflow-engine/
    │   └── application.yaml
    ├── cnpg-cloud/
    │   └── application.yaml
    ├── cnpg-site/
    │   └── application.yaml
    ├── cnpg-temporal/
    │   └── application.yaml
    ├── kubernetes-replicator/
    │   └── application.yaml
    ├── nke-storage/
    │   ├── storage-classes.yaml
    │   └── persistent-volumes.yaml
    └── temporal/
        └── application.yaml
```

To use it:

1. Fork or copy the reference repo into your own git repository
2. Edit each subdirectory under `deploy/` to update chart versions, Helm values, and storage manifests for your environment
3. Point `tier0-bootstrap` at your repo:

```yaml
argocd:
  repos:
    - url: "https://gitlab.example.com/my-org/my-nke-deploy.git"
      token: "glpat-xxxxxxxxxxxx"
      deployPaths:
        - "deploy"
```

### Key Values to Customize

When adapting the reference manifests:

- **Chart versions** (`spec.source.targetRevision`) — pin to versions you've tested
- **Container image tags** — CWE image tag in its Helm values
- **Database storage** — adjust `storageClass` and `size` for your storage backend
- **Database credentials** — override the default passwords for production via CNPG secrets
- **Cluster instances** — CNPG `cluster.instances` (3 for HA, 1 for single-node)

### NGC Helm Repository Secrets

The Application manifests reference `https://helm.ngc.nvidia.com/j7sbcjl3qgta`. ArgoCD needs credentials to pull from this registry. When using the `nke-base-config` template item, NGC Helm repo secrets are created automatically. If you remove `nke-base-config`, create them yourself:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: repo-helm-ngc-j7sbcjl3qgta
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: j7sbcjl3qgta
  url: https://helm.ngc.nvidia.com/j7sbcjl3qgta
  type: helm
  username: $oauthtoken
  password: "<your-ngc-api-key>"
```

---

## `database`

CNPG PostgreSQL credentials. Use strong unique passwords for production deployments.

| Sub-key | Env | Description |
|---------|-----|-------------|
| `superuserPassword` | `CNPG_SUPERUSER_PASSWORD` | CNPG superuser password |
| `cloudPassword` | `CNPG_CLOUD_PASSWORD` | Cloud DB application password |
| `sitePassword` | `CNPG_SITE_PASSWORD` | Site DB application password |
| `temporalPassword` | `CNPG_TEMPORAL_PASSWORD` | Temporal DB password |

```yaml
database:
  superuserPassword: "<strong-password-1>"
  cloudPassword: "<strong-password-2>"
  sitePassword: "<strong-password-3>"
  temporalPassword: "<strong-password-4>"
```

---

## `monitoring`

Grafana remote-write and Loki push credentials. All fields are optional — if omitted, the corresponding monitoring secrets are not created.

| Sub-key | Env | Description |
|---------|-----|-------------|
| `prometheusUsername` | `GRAFANA_PROMETHEUS_USERNAME` | Prometheus remote-write username |
| `prometheusPassword` | `GRAFANA_PROMETHEUS_PASSWORD` | Prometheus remote-write password |
| `lokiUsername` | `GRAFANA_LOKI_USERNAME` | Loki push username |
| `lokiPassword` | `GRAFANA_LOKI_PASSWORD` | Loki push password |
| `lokiHost` | `GRAFANA_LOKI_HOST` | Loki push endpoint |

```yaml
monitoring:
  prometheusUsername: "user"
  prometheusPassword: "pass"
  lokiUsername: "user"
  lokiPassword: "pass"
  lokiHost: "https://loki.example.com"
```

---

## `kubevip`

kube-vip configuration for high availability. When enabled, a virtual IP is shared across control plane nodes.

| Sub-key | Env | Type | Default | Description |
|---------|-----|------|---------|-------------|
| `enabled` | `KUBEVIP_ENABLED` | boolean | `false` | Enable kube-vip |
| `vip` | `KUBEVIP_VIP` | IP string | — | Virtual IP address (required when enabled) |
| `version` | `KUBEVIP_VERSION` | string | `v0.8.0` | kube-vip version |
| `interface` | `KUBEVIP_INTERFACE` | string | — | Network interface |
| `vipRanges` | `KUBEVIP_VIP_RANGES` | string | — | Comma-separated VIP CIDR ranges |

```yaml
kubevip:
  enabled: true
  vip: "10.0.0.100"
  vipRanges: "10.0.0.100/32"
```

Setting `--kubevip-vip` on the CLI implies `--kubevip-enabled`.

---

## `forgeApiKey`

`FORGE_API_KEY`

Infrastructure provider API key. Required when using a Forge or NICo-backed infrastructure provider. Omit when `baremetalMode` is enabled.

Prefer setting this via the `FORGE_API_KEY` environment variable.

---

## `baremetalMode`

`BAREMETAL_MODE`

Skip all infrastructure provider API calls. When enabled, `forgeApiKey` is not required, but you must provide site metadata manually.

**Type:** boolean  
**Default:** `false`

```yaml
baremetalMode: true
siteId: "my-site-id"
vpcId: "my-vpc-id"
```

Related fields: `siteId` (`SITE_ID`), `vpcId` (`VPC_ID`), `instanceTypeId` (`INSTANCE_TYPE_ID`), `subnetIds` (`SUBNET_IDS`), `sshKeyGroupIds` (`SSH_KEY_GROUP_IDS`).

---

## `enableMtls`

> **Deprecated.** This flag enables mTLS via Kubetrust, which is being replaced by SPIFFE/SPIRE. Future builds enable mTLS automatically. Do not use for new deployments.

**Type:** boolean  
**Default:** `false`

---

## Examples

### Single Repo (External Team)

```yaml
clusterName: "my-cluster"

registry:
  ngcApiKey: ""  # Set via NGC_API_KEY env var

argocd:
  repos:
    - url: "https://gitlab.example.com/team/my-nke-deploy.git"
      token: ""  # Set via env var or CLI
      username: "oauth2"
      deployPaths:
        - "deploy"
```

### Baremetal Mode

```yaml
clusterName: "baremetal-cluster"
baremetalMode: true
siteId: "my-site-id"
vpcId: "my-vpc-id"

registry:
  ngcApiKey: ""

argocd:
  repos:
    - url: "https://gitlab.example.com/team/deploy.git"
      token: ""
      deployPaths:
        - "environments/prod"
```

### HA with kube-vip

```yaml
clusterName: "my-ha-cluster"

cluster:
  k8sVersion: "1.34.2"
  cniName: "calico"
  joinNodes:
    - name: "ctrl-2"
      ip: "10.0.0.11"
    - name: "ctrl-3"
      ip: "10.0.0.12"

kubevip:
  enabled: true
  vip: "10.0.0.100"
  vipRanges: "10.0.0.100/32"

registry:
  ngcApiKey: ""

argocd:
  repos:
    - url: "https://gitlab.example.com/team/deploy.git"
      token: ""
      deployPaths:
        - "deploy"
```
