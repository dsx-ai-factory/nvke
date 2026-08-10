# Argo CD Deployment Bundle

Bundler Version: v0.18.0-next
Recipe Version: v0.18.0-next

## Overview

This bundle contains Argo CD Application manifests for deploying NVIDIA AI Cluster Runtime components using the App of Apps pattern.

## Components

The following components are included in deployment order:

| Component | Type | Version | Sync Wave | Namespace |
|-----------|------|---------|-----------|-----------|
| cert-manager | Helm | v1.15.1 | 1 | cert-manager |
| cloudnative-pg | Helm | 0.22.1-nke.2 | 5 | cnpg-system |
| cnpg-cloud | Helm | 0.3.0 | 9 | cnpg-cloud |
| cnpg-site | Helm | 0.3.0 | 9 | cnpg-site |
| cnpg-temporal | Helm | 0.3.0 | 9 | cnpg-temporal |
| external-secrets | Helm | 0.13.0-nke.1 | 1 | external-secrets |
| fluent-bit | Helm | 0.47.9-nke.1 | 1 | fluent-bit |
| gateway-api-crds | Helm | 0.1.0 | 1 | envoy-gateway-system |
| envoy-gateway | Helm | v1.8.2-nke.2 | 5 | envoy-gateway-system |
| kube-prometheus-stack | Helm | 68.0.0 | 5 | monitoring |
| dgxc-alloy | Helm | 0.28.0-nke.1 | 9 | dgxc-alloy |
| kubernetes-replicator | Helm | 2.10.2 | 1 | kubernetes-replicator |
| cluster-secrets | Helm | 0.3.0 | 5 | cluster-secrets |
| l0-nke-monitor | Helm | 0.1.1 | 9 | platform |
| longhorn | Helm | 1.10.1 | 1 | longhorn-system |
| nke-storage | Helm | 0.4.0 | 5 | longhorn-system |
| nvault-k8s | Helm | 1.1.0 | 1 | nvault-k8s |
| prometheus-adapter | Helm | 5.3.0 | 9 | monitoring |
| reloader | Helm | 2.2.9 | 1 | reloader |
| skyhook-operator | Helm | v0.9.2-nke.2 | 1 | skyhook |
| nke-skyhooks | Helm | 0.1.1 | 5 | skyhook |
| spire-crds | Helm | 0.5.0 | 1 | spire-mgmt |
| nke-spire | Helm | 0.1.3 | 5 | spire-server |
| teleport-kube-agent | Helm | 18.2.5-nke.1 | 1 | dgxc-teleport |
| nke-teleport-apps | Helm | 0.2.0 | 5 | dgxc-teleport |
| tbot | Helm | 18.2.5-nke.1 | 5 | cloud-workflow-engine |
| temporal | Helm | 0.54.0 | 13 | temporal |
| cloud-workflow-engine | Helm | 0.0.620 | 17 | cloud-workflow-engine |
| site-workflow-engine | Helm | 0.9.4 | 17 | site-workflow-engine |
| zipkin | Helm | 0.3.4 | 1 | zipkin |

## Prerequisites

- Kubernetes cluster with Argo CD installed
- Argo CD CLI (`argocd`) configured
- Git repository for storing these manifests
- kubectl configured with cluster access

## Deployment Steps

### 1. Prepare Git Repository

Push this bundle to your GitOps repository:

```bash
cd <bundle-directory>
git init
git add .
git commit -m "Add NVIDIA AI Cluster Runtime manifests"
git remote add origin YOUR_REPO_URL
git push -u origin main
```

### 2. Update Repository URL

`repoURL` is baked into both `app-of-apps.yaml` *and* every per-component
`NNN-<component>/application.yaml` (path-based child Applications inherit
the bundle's repo URL — Argo CD does not propagate the parent's `repoURL`
to children). Update all of them in one pass:

```bash
# Update repoURL across the parent and every per-component application.yaml
find . -name 'application.yaml' -o -name 'app-of-apps.yaml' \
  | xargs sed -i 's|https://github.com/YOUR-ORG/YOUR-REPO.git|YOUR_ACTUAL_REPO_URL|g'
```

To avoid the rewrite, regenerate the bundle with `--repo` so the URL is
populated at bundle time:

```bash
aicr bundle --deployer argocd --repo YOUR_ACTUAL_REPO_URL ...
```

### 3. Apply App of Apps

```bash
kubectl apply -f app-of-apps.yaml
```

### 4. Monitor Deployment

```bash
# Watch application sync status
argocd app list

# Get detailed status for a specific application
argocd app get nvidia-stack

# Watch sync progress
argocd app sync nvidia-stack --watch
```

## Directory Structure

Each `NNN-<component>/` folder is one of two kinds, distinguished by the
presence of `Chart.yaml`. KindUpstreamHelm folders (no `Chart.yaml`) carry
just `values.yaml` + `application.yaml` — the Application is multi-source
(upstream Helm repo + your git repo for values). KindLocalHelm folders
(`Chart.yaml` + `templates/`) carry the wrapped chart bytes plus a
path-based single-source `application.yaml`.

```text
<bundle-directory>/
├── app-of-apps.yaml           # Parent application
├── README.md                  # This file
├── 001-cert-manager/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 002-cloudnative-pg/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 003-cnpg-cloud/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 9)
├── 004-cnpg-site/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 9)
├── 005-cnpg-temporal/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 9)
├── 006-external-secrets/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 007-fluent-bit/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 008-gateway-api-crds/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 009-envoy-gateway/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 010-kube-prometheus-stack/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 011-dgxc-alloy/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 9)
├── 012-kubernetes-replicator/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 013-cluster-secrets/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 014-l0-nke-monitor/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 9)
├── 015-longhorn/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 016-nke-storage/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 017-nvault-k8s/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 018-prometheus-adapter/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 9)
├── 019-reloader/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 020-skyhook-operator/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 021-nke-skyhooks/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 022-spire-crds/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 023-nke-spire/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 024-teleport-kube-agent/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 025-nke-teleport-apps/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 026-tbot/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 5)
├── 027-temporal/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 13)
├── 028-cloud-workflow-engine/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 17)
├── 029-site-workflow-engine/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 17)
├── 030-zipkin/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
```

`--dynamic` is not supported with `--deployer argocd` — use `--deployer
argocd-helm` for install-time values. The helm-deployer files
(`install.sh`, `upstream.env`, `cluster-values.yaml`) are intentionally
omitted; Argo's repo-server never consumes them.

## Sync Waves

Components are deployed in order using Argo CD sync-waves:
- **Wave 1**: cert-manager
- **Wave 5**: cloudnative-pg
- **Wave 9**: cnpg-cloud
- **Wave 9**: cnpg-site
- **Wave 9**: cnpg-temporal
- **Wave 1**: external-secrets
- **Wave 1**: fluent-bit
- **Wave 1**: gateway-api-crds
- **Wave 5**: envoy-gateway
- **Wave 5**: kube-prometheus-stack
- **Wave 9**: dgxc-alloy
- **Wave 1**: kubernetes-replicator
- **Wave 5**: cluster-secrets
- **Wave 9**: l0-nke-monitor
- **Wave 1**: longhorn
- **Wave 5**: nke-storage
- **Wave 1**: nvault-k8s
- **Wave 9**: prometheus-adapter
- **Wave 1**: reloader
- **Wave 1**: skyhook-operator
- **Wave 5**: nke-skyhooks
- **Wave 1**: spire-crds
- **Wave 5**: nke-spire
- **Wave 1**: teleport-kube-agent
- **Wave 5**: nke-teleport-apps
- **Wave 5**: tbot
- **Wave 13**: temporal
- **Wave 17**: cloud-workflow-engine
- **Wave 17**: site-workflow-engine
- **Wave 1**: zipkin

## Customization

### Modifying Values

Edit the `values.yaml` file in each Helm component directory to customize the deployment.

### Changing Deployment Order

Modify the `sync-wave` annotation in each `application.yaml` to change deployment order.

## Troubleshooting

### Application Not Syncing

```bash
# Check application status
argocd app get <app-name>

# Force sync
argocd app sync <app-name> --force

# View application logs
argocd app logs <app-name>
```

### Resource Conflicts

If resources already exist, you may need to adopt them:

```bash
argocd app sync <app-name> --replace
```

## References

- [Argo CD Documentation](https://argo-cd.readthedocs.io/)
- [App of Apps Pattern](https://argo-cd.readthedocs.io/en/stable/operator-manual/cluster-bootstrapping/)
- [Sync Waves](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-waves/)
