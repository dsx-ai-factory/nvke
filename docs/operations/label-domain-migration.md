# The `nvke.nvidia.com` Label and Annotation Domain

NVKE writes the labels, annotations, taints and node conditions it owns under
`nvke.nvidia.com/`. It used to write them under `nke.nvidia.com/`, and it still
*reads* that form everywhere, so an existing site needs no relabelling step and
no maintenance window.

New installs need none of this. The notes below are for an existing site and
for anything outside NVKE that sets or watches these keys.

## What changed

| Where | Before | After |
|---|---|---|
| Written by NVKE | `nke.nvidia.com/<name>` | `nvke.nvidia.com/<name>` |
| Read by NVKE | `nke.nvidia.com/<name>` | both, new form preferred |
| Deleted by NVKE | `nke.nvidia.com/<name>` | both |

The names after the domain are unchanged, so `nke.nvidia.com/reboot` becomes
`nvke.nvidia.com/reboot` and nothing else about it moves.

## What you do not have to do

- **Relabel existing objects.** NVKE reads both domains. An object labelled by
  an earlier build keeps working as it is.
- **Coordinate the upgrade with anything else.** Every reader in NVKE accepts
  both forms, in either order of rollout.
- **Change how you request a reboot or a replacement.** `nke.nvidia.com/reboot`
  and `nke.nvidia.com/terminate` are still honoured. Prefer the new form in new
  automation; the old one is not going away in this release.

Objects converge on the new domain on their own as NVKE rewrites them. Node
topology labels move on the next reconcile of the node; kubeconfig Secret
ownership labels move the next time the Secret is published; the provisioning
taint is written under the new domain by the next node to join.

## What you do have to check

**`cluster-api-provider-nico`'s reboot and repair flags.** This is the one key
pair with a single configured reader. CAPNICo watches exactly one annotation
for each, named by `--reboot-annotation` and `--repair-annotation`, and
silently ignores a request under any other key — a mismatch produces no error
anywhere, it just means reboot and repair requests are never actioned.

NVKE therefore writes those two annotations under **both** domains while sites
move over, so a CAPNICo still pointed at `nke.nvidia.com/*` keeps working. The
manifests in this repository set the new values:

```yaml
- --reboot-annotation=nvke.nvidia.com/reboot
- --repair-annotation=nvke.nvidia.com/machine-health-issue
```

If your site deploys CAPNICo from its own manifests, move those flags across at
your convenience. The dual write goes away once every deployment has moved
([#1718](https://github.com/dsx-ai-factory/nvke/issues/1718)), and after that a
stale flag means reboot and repair requests are silently dropped. Verify what is
actually running:

```sh
kubectl -n capnico-system get deployment controller-manager \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].args}'
```

**Anything outside NVKE that reads these keys.** Dashboards, alerting rules,
scripts and break-fix automation that match on `nke.nvidia.com/...` keep
working against objects NVKE has not yet rewritten, and stop matching once it
has. Match on both while you migrate, for example:

```promql
kube_node_labels{label_nvke_nvidia_com_site_name!=""} or kube_node_labels{label_nke_nvidia_com_site_name!=""}
```

Anything that sets `NeedsRecycle` or `NeedsRepair`, such as a break-fix system, may use
either domain; NVKE reads both.

## What did not change

These carry `nke.nvidia.com` in their names and are **not** part of this
change, because renaming them is a separate, breaking operation:

| Name | Why it stays |
|---|---|
| `auth.nke.nvidia.com/v1alpha1` | An API group, not a label domain |
| `owner.nke.nvidia.com/<kind>` | Informer label selectors cannot match two keys at once, so a rename would hide every object an earlier build labelled |
| `controller.nke.nvidia.com`, `management-engine.nke.nvidia.com` | Leader-election IDs; changing one mid-rollout lets two leaders run at once |
| `internal.nke.nvidia.com` | Covered by [crd-group-migration.md](crd-group-migration.md) |
