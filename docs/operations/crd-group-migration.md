# Migrating CRs to the `internal.nvke.nvidia.com` Group

The management-engine's internal API group changed from `internal.nke.nvidia.com`
to `internal.nvke.nvidia.com`, and its cluster kind from `NKECluster` to
`NVKECluster`, as part of the rename to NVKE.

**A group change is a new CRD, not an edit.** There is no conversion webhook and
no automatic migration: the new CRDs install alongside the old ones, and the old
custom resources stay where they are until you copy them across. A site that
upgrades without doing this comes up with a management-engine that sees no
clusters and no releases.

New installs need none of this. The procedure below is for an existing site.

## What moves

| Kind (old → new) | Scope | Status subresource |
|---|---|---|
| `NKECluster` → `NVKECluster` | Namespaced | Yes |
| `ClusterRelease` | Cluster | Yes |
| `NodePoolRelease` | Cluster | Yes |
| `MachineImage` | Cluster | Yes |
| `ClusterReleaseChannel` | Cluster | No |

Only the cluster kind is renamed. The other four keep their names and change
group only.

The finalizer `internal.nke.nvidia.com/cluster-cleanup` becomes
`internal.nvke.nvidia.com/cluster-cleanup`. You do not add the new one by hand —
the controller sets it on first reconcile.

## Before you start

- Take a backup of the old resources. Step 2 writes one, and it is the only copy
  once step 6 deletes the originals.
- `jq` and a `kubectl` that can reach the site controller.
- Know your NVKECluster namespaces. The examples use `--all-namespaces`.

> **Status matters.** `NVKECluster.status.provisioning` pins a cluster to the
> CWE cluster it created: `endpoint` is the service that provisioned it and `id`
> is its UUID. The reconciler falls back to a lookup by name when the status is
> absent, so a lost status does not by itself double-provision — but `id` is
> also what stops a *deleted* cluster being silently replaced by a new one at
> the same name. Copy the status. Step 4 does.

## Procedure

### 1. Stop the management-engine

Two controllers reconciling the same clusters through different CRDs will fight.

```bash
kubectl -n nke-system scale deployment/management-engine --replicas=0
kubectl -n nke-system rollout status deployment/management-engine --timeout=120s
```

### 2. Export the existing resources

```bash
mkdir -p crd-group-backup && cd crd-group-backup

kubectl get nkeclusters.internal.nke.nvidia.com --all-namespaces -o json > nkeclusters.json
for k in clusterreleases nodepoolreleases machineimages clusterreleasechannels; do
  kubectl get "${k}.internal.nke.nvidia.com" -o json > "${k}.json"
done
```

Confirm the export is not empty before going further:

```bash
for f in *.json; do echo "${f}: $(jq '.items | length' "${f}")"; done
```

### 3. Rewrite and apply the specs

`jq` drops the server-owned metadata that cannot be re-applied — `uid`,
`resourceVersion`, `generation`, `creationTimestamp`, `managedFields`,
`selfLink` — and the old finalizer, which would otherwise be carried onto an
object whose controller never set it.

```bash
rewrite() {  # $1 = input file, $2 = new apiVersion, $3 = new kind (empty keeps it)
  jq --arg av "$2" --arg kind "$3" '
    .items
    | map(
        .apiVersion = $av
        | (if $kind != "" then .kind = $kind else . end)
        | del(
            .metadata.uid, .metadata.resourceVersion, .metadata.generation,
            .metadata.creationTimestamp, .metadata.managedFields,
            .metadata.selfLink, .metadata.ownerReferences
          )
        | .metadata.finalizers = (
            (.metadata.finalizers // [])
            | map(select(startswith("internal.nke.nvidia.com/") | not))
          )
        | if (.metadata.finalizers | length) == 0 then del(.metadata.finalizers) else . end
      )
    | {apiVersion: "v1", kind: "List", items: .}
  ' "$1"
}

rewrite nkeclusters.json         internal.nvke.nvidia.com/v1 NVKECluster > new-nvkeclusters.json
for k in clusterreleases nodepoolreleases machineimages clusterreleasechannels; do
  rewrite "${k}.json" internal.nvke.nvidia.com/v1 "" > "new-${k}.json"
done
```

Apply the releases first: a cluster resolves its release channel on reconcile,
and a channel that is not there yet reconciles to a failure you then have to
clear.

```bash
for k in machineimages clusterreleases nodepoolreleases clusterreleasechannels; do
  kubectl apply -f "new-${k}.json"
done
kubectl apply -f new-nvkeclusters.json
```

### 4. Restore the status subresources

`kubectl apply` does not write a status subresource. Patch each one from the
backup.

```bash
restore_status() {  # $1 = backup file, $2 = new plural.group, $3 = "ns" when namespaced
  jq -r '.items[] | select(.status != null)
         | [.metadata.namespace // "", .metadata.name, (.status | tojson)] | @tsv' "$1" \
  | while IFS=$'\t' read -r ns name status; do
      if [ "$3" = "ns" ]; then
        kubectl -n "$ns" patch "$2" "$name" --subresource=status \
          --type=merge -p "{\"status\":${status}}"
      else
        kubectl patch "$2" "$name" --subresource=status \
          --type=merge -p "{\"status\":${status}}"
      fi
    done
}

restore_status nkeclusters.json      nvkeclusters.internal.nvke.nvidia.com     ns
for k in clusterreleases nodepoolreleases machineimages; do
  restore_status "${k}.json" "${k}.internal.nvke.nvidia.com"
done
```

`ClusterReleaseChannel` has no status subresource and is skipped.

### 5. Verify before deleting anything

Counts match, and every cluster kept its provisioning identity:

```bash
kubectl get nvkeclusters.internal.nvke.nvidia.com --all-namespaces
kubectl get nvkeclusters.internal.nvke.nvidia.com --all-namespaces \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.provisioning.id}{"\n"}{end}'
```

Every row must carry the same UUID as the matching row in `nkeclusters.json`. An
empty `id` on a cluster that had one means step 4 did not land — fix it before
continuing, not after.

Bring the controller back and let it reconcile:

```bash
kubectl -n nke-system scale deployment/management-engine --replicas=1
kubectl -n nke-system rollout status deployment/management-engine --timeout=300s
kubectl -n nke-system logs deployment/management-engine --tail=100
```

Each `NVKECluster` should reach `Ready` and pick up the new finalizer:

```bash
kubectl get nvkeclusters.internal.nvke.nvidia.com --all-namespaces \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.finalizers}{"\n"}{end}'
```

**No tenant cluster should restart, re-provision or change state.** If one does,
stop and restore from `crd-group-backup` rather than letting reconciliation run on.

### 6. Remove the old resources

Only once step 5 is clean. The old controller is gone, so nothing will remove
the old finalizers — strip them first or the delete hangs forever.

```bash
kubectl get nkeclusters.internal.nke.nvidia.com --all-namespaces \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
| while read -r ns name; do
    kubectl -n "$ns" patch nkeclusters.internal.nke.nvidia.com "$name" \
      --type=merge -p '{"metadata":{"finalizers":null}}'
  done
```

Deleting the old CRDs deletes the custom resources with them:

```bash
for c in nkeclusters clusterreleases nodepoolreleases machineimages clusterreleasechannels; do
  kubectl delete crd "${c}.internal.nke.nvidia.com" --ignore-not-found
done
```

The chart marks these CRDs `helm.sh/resource-policy: keep`, so a Helm upgrade
will not remove them for you. This step is deliberate and manual.

Keep `crd-group-backup` until the site has run a full provisioning cycle on the
new group.

## Rollback

Before step 6, rollback is: scale the management-engine to zero, delete the new
CRs, reinstall the previous chart, scale back up. The old CRs were never touched.

After step 6 the old CRDs are gone and rollback means re-installing the previous
chart and replaying `crd-group-backup` through the same procedure in reverse. This
is why step 5 verifies before step 6 deletes.
