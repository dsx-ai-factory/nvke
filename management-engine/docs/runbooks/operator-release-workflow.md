# Operator release workflow

This runbook guides operators through publishing releases, managing release
channels and validating rollout through the Cluster Service APIs. Administrators
can provide additional checks when direct access to the datastore or workload
cluster is needed.

This runbook covers the routine release workflow:

1. Publish a new release or select an existing one.
2. Update the intended channel so eligible clusters following it adopt the release.
3. Monitor the resulting cluster operations and verify completion.
4. Diagnose failures and retire unused releases when appropriate.

Publishing adds a catalog record. Updating a channel changes the releases available
to clusters following it, according to their version selection and eligibility
rules. A cluster can also be moved to another channel or version selection.

Administrators building node images and configuring the NVKE controller's
lower-level catalog should use the [node-image guide](../../../docs/operate/build-node-images.md).
That catalog is separate from the one managed through the Cluster Service APIs.

For container images and Helm charts, see [package publication](../artifact-publication.md).
For validation on one cluster, see [test a release before wider rollout](#test-a-release-before-wider-rollout).

## Before starting

You need `curl`, `jq` and access to the Cluster Service APIs. Confirm that
Cluster Service meets the [installation prerequisites](../helm-installation.md)
and can reach the NVKE controller.

Use the site's tenant API endpoint and its approved internal
endpoint for the operator API. Both refer to Cluster Service. Add the authentication
options required by your deployment to each curl command. Before exposing either
API, verify that authentication and authorization are enforced; deploying the
workloads alone does not establish access control.

| Variable | Value |
| --- | --- |
| `TENANT_API_URL` | Published tenant-facing Cluster Service base URL, without a trailing slash or `/v1`. |
| `OPERATOR_API_URL` | Approved internal operator-facing Cluster Service base URL, without a trailing slash or `/v1`. |
| `CLUSTER_NAME` | Cluster to inspect or change through the tenant API. |

Use an operator identity with catalog read/write access and an authorized tenant
identity with read/update access to the target cluster. Cluster observations come
from the Cluster Service tenant API; the operator API manages the catalog.
Keep credentials out of Git and captured results.

## Select the release inputs

Choose an existing machine image and identify the channel to update:

```sh
curl -i "${OPERATOR_API_URL:?}/v1/machine-images"
curl -i "${OPERATOR_API_URL:?}/v1/cluster-release-channels"
```

For a new release, select its name, image, Kubernetes version and approved
bootstrap configuration from the release artifacts. The selector identifies the
version offered through the channel; minimum upgrade age controls when a new
release is eligible.

| Variable | Value |
| --- | --- |
| `RELEASE_NAME` | New or existing release to make available. |
| `MACHINE_IMAGE_NAME` | Existing machine image from the catalog. |
| `KUBERNETES_VERSION` | Exact Kubernetes version the release should boot. |
| `BOOTSTRAP_CONFIG` | JSON file containing the approved `machineInit.infra` object: bootstrap files and pre-kubeadm commands. |
| `CHANNEL_NAME` | Existing channel to update or select for a cluster. |
| `VERSION_SELECTOR` | Channel selector: `major.minor` allows patch upgrades; `major.minor.patch` selects an exact Kubernetes version. |
| `MINIMUM_UPGRADE_AGE` | Minimum release age before existing clusters may adopt it, such as `24h`. |

Use the schema from the installed version. The [release definition](../../apis/v1/clusterrelease_types.go)
and [initialization fields](../../apis/v1/common_types.go) describe optional settings,
including additional bootstrap files and commands. The current schema requires
a cloud-init header in `userDataTemplate`; node setup on the CAPI path comes
from `machineInit.infra`.

Use an image and initialization configuration tested together for the target
provider and Kubernetes version. Include the required networking and identity
setup.

## Publish or select a release

Check that the release name is available:

```sh
curl -i "${OPERATOR_API_URL:?}/v1/cluster-releases/${RELEASE_NAME:?}"
```

If the response is HTTP 200, compare the existing release with the intended
inputs. If they match, use that release and continue to [update the channel](#update-the-channel).
Otherwise, choose a new name. Resolve lookup errors before continuing.

Write the request to `release.json` and inspect it:

```sh
jq -n \
  --arg name "${RELEASE_NAME:?}" \
  --arg image "${MACHINE_IMAGE_NAME:?}" \
  --arg version "${KUBERNETES_VERSION:?}" \
  --slurpfile bootstrap "${BOOTSTRAP_CONFIG:?}" \
  '{metadata: {name: $name}, spec: {
    kubernetes: {version: $version},
    machineImageRef: {name: $image},
    machineInit: {userDataTemplate: "#cloud-config\n", infra: $bootstrap[0]}
  }}' > release.json
cat release.json
```

Confirm the request includes the files and commands required by the image, then
publish it:

```sh
curl -i -H 'Content-Type: application/json' \
  --data-binary @release.json \
  "${OPERATOR_API_URL:?}/v1/cluster-releases"
```

Expect HTTP 201 and the created release. Changing the Kubernetes version, image
or initialization requires a new release name.

## Update the channel

Updating the channel makes the release available to clusters following the affected
version selection. These clusters adopt eligible releases without individual edits.
Review which clusters follow this channel before making the change.

Fetch the channel and display its response headers and JSON:

```sh
curl -D - -o channel.json \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels/${CHANNEL_NAME:?}"
cat channel.json
```

Copy the response's `ETag` value into `CHANNEL_ETAG`, keeping the double quotes.
The API requires it in `If-Match` so the update cannot overwrite a newer change.

Add the release as the first candidate, keeping the previous candidates and
other selectors unchanged. Retain an eligible reference to each cluster's
current release until those clusters have moved off it. Otherwise, reconciliation
can fail while the new candidate is too young to adopt. Clusters with no current
release have no previous reference to retain.

The following request prepends the candidate and removes any duplicate reference
to it. Review `channel.json` and the intended candidate order before submitting:

```sh
curl -i -X PUT \
  -H "If-Match: ${CHANNEL_ETAG:?}" -H 'Content-Type: application/json' \
  --data-binary "$(jq --arg selector "${VERSION_SELECTOR:?}" \
    --arg release "${RELEASE_NAME:?}" --arg age "${MINIMUM_UPGRADE_AGE:?}" \
    '{metadata: (.metadata | {name, uid}), spec}
     | .spec.releases[$selector] =
       ([{name: $release, minimumUpgradeAge: $age}]
        + [(.spec.releases[$selector] // [])[] | select(.name != $release)])' \
    channel.json)" \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels/${CHANNEL_NAME:?}"
```

Expect HTTP 200. A stale ETag returns HTTP 412: read the resource and review the
change again. Updates accept `metadata.name`, optional `metadata.uid` and `spec`;
timestamps and status are read-only.

Monitor adopting clusters using the [rollout checks](#verify-the-rollout).
Adoption depends on each cluster's selection and the release's eligibility; a
channel update does not mean every cluster upgrades immediately. A previous
release can be marked `spec.lifecycle.deprecated: true` to signal that existing
consumers should migrate. Keep its channel reference while those consumers still
need it; deprecation and deletion are separate operations.

## Change a cluster's release selection

Use this when moving an individual cluster to another channel or version selection.
Clusters already following the intended channel need no selection change.
Fetch the cluster before updating it:

```sh
curl -D - -o cluster.json "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
cat cluster.json
```

Copy the response's `ETag` value into `CLUSTER_ETAG`, keeping the double quotes.
Write the updated selection to `cluster-update.json`:

```sh
jq --arg channel "${CHANNEL_NAME:?}" --arg version "${VERSION_SELECTOR:?}" \
  '{metadata: (.metadata | {name, uid}), spec}
   | .spec.release.channel = {name: $channel, version: $version}' \
  cluster.json > cluster-update.json
cat cluster-update.json
```

Review the complete spec, including the unchanged node pools, then submit it:

```sh
curl -i -X PUT \
  -H "If-Match: ${CLUSTER_ETAG:?}" -H 'Content-Type: application/json' \
  --data-binary @cluster-update.json \
  "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
```

Expect HTTP 200. The manager resolves the selection subject to channel eligibility
and minimum upgrade age. A successful API response means the request was stored.

Worker releases are selected separately through [node profiles](../../apis/v1/nodeprofile_types.go).
Set `NODE_POOL_NAME` to the existing pool and `NODE_PROFILE` to its approved
`profile:tag`. Fetch `cluster.json` again and copy its new ETag into `CLUSTER_ETAG`.
Use this command to change the worker profile, then submit `cluster-update.json`
with the PUT command above:

```sh
jq --arg pool "${NODE_POOL_NAME:?}" --arg profile "${NODE_PROFILE:?}" \
  'if any(.spec.nodePools[]?; .name == $pool) then
     {metadata: (.metadata | {name, uid}), spec}
     | (.spec.nodePools[] | select(.name == $pool) | .nodeProfile) = $profile
   else error("node pool not found") end' \
  cluster.json > cluster-update.json
cat cluster-update.json
```

## Verify the rollout

For each affected cluster, set `CLUSTER_NAME` and query the Cluster Service tenant
API using an identity authorized for that tenant:

```sh
curl -i "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
```

Check the returned selection and service-reported progress:

| Check | Expected result |
| --- | --- |
| `spec.release` | Matches the intended channel and version selection. |
| `status.currentRelease` | Names the intended control-plane release and Kubernetes version. This records backend configuration, not completed node replacement. |
| `status.conditions` | `Synced`, `ControlPlaneReady` and `Ready` are `True`. Inspect reasons and messages for incomplete or failed operations. |
| Reported generation | Relevant conditions agree with `status.observedGeneration`; mismatches indicate observations from different revisions. |
| Backend operation | `NVKEControllerProvisioned=True` and `NVKEControllerProvisioning=False`. For requested upgrades or recycling, wait for the corresponding progress conditions to become `False` and inspect any `NVKEControllerUpdateFailed` error. Unknown status does not confirm completion. |
| `status.nodePools` | Each requested pool reports the intended release and readiness. Where replica counts are populated, compare them with the requested counts; omitted counts mean unknown. |

The tenant API omits `metadata.generation`, so these checks cannot independently
prove that every observation reflects the latest spec revision. A matching
`currentRelease` or `Ready=True` also does not establish that every node has been
replaced or that a new image has booted. If status is inconclusive, or independent
node-level verification is required, use the
[administrator-assisted checks](#administrator-assisted-checks).

## Diagnose failures

Read the conditions, reasons and messages returned by the cluster GET above.
Record the intended selection, resolved release and backend cluster ID
(`status.id`) when escalating to the service, backend or provider owner.

| Symptom | Next action |
| --- | --- |
| API unavailable or HTTP 401/403 | Check the endpoint and caller credentials/permissions. Involve the service administrator if the API remains unavailable. |
| HTTP 412 on update | Fetch the resource again, review changes and rebuild the request with its new ETag. |
| Release not selected | Check the channel/version selection, minimum upgrade age and release lifecycle fields. Correct those inputs before retrying. |
| Nodes fail to initialize or become Ready | Ask the administrator or backend owner to inspect bootstrap and agent logs, networking, workload identity and image/CNI compatibility. |
| `ClusterReconciled=False` or `NodePoolsReconciled=False` | Check referenced catalog resources through the APIs. For infrastructure-template or service configuration errors, involve the administrator. |
| `NVKEControllerUpdateFailed=True`, stalled replacement or nodes not Ready | Capture the API status and involve the backend/provider owner before making another version change. Request node-level checks if needed. |

For a bad release, stop promoting it to other channels while investigating. A
channel edit or Helm rollback does not undo a workload cluster upgrade. Follow
the [workload upgrade guidance](../../../docs/workload-clusters/upgrade.md) for supported
control-plane and worker operations; recovery from a partially completed upgrade
needs the backend/provider owner's assessment. For package or datastore problems,
see [upgrade and state retention](../helm-installation.md#crd-ownership-upgrades-and-retention).

## Retire unused releases

Retain previous releases for the agreed recovery period. Set `RETIRED_RELEASE_NAME`
to the release to remove and inspect it:

```sh
curl -i "${OPERATOR_API_URL:?}/v1/cluster-releases/${RETIRED_RELEASE_NAME:?}"
```

Check all channel and cluster references, including any paginated results, and
confirm the release is no longer in use. Remove remaining channel references.
Copy its `ETag` into `RELEASE_ETAG`, keeping the double quotes, then delete the
release record:

```sh
curl -i -X DELETE -H "If-Match: ${RELEASE_ETAG:?}" \
  "${OPERATOR_API_URL:?}/v1/cluster-releases/${RETIRED_RELEASE_NAME:?}"
```

Expect HTTP 202, then repeat the GET until it returns HTTP 404. Retain shared
images and other resources still in use.

Record package and image references, cluster and release identities, API results,
observed outcome and any recovery actions. Keep the generated response files for
review according to the site's data-handling rules.

## Test a release before wider rollout

Use a test cluster with the same provider and configuration as the intended
targets. Keep its current release available for comparison and recovery. Validate
initial provisioning separately if the image or initialization is new.

To validate a candidate on one cluster, [publish or select the release](#publish-or-select-a-release)
and set `TEST_CHANNEL_NAME` to a new channel reserved for that cluster. Create it
with the candidate release and no minimum age delay:

```sh
jq -n --arg name "${TEST_CHANNEL_NAME:?}" \
  --arg selector "${VERSION_SELECTOR:?}" --arg release "${RELEASE_NAME:?}" \
  '{metadata: {name: $name}, spec: {releases: {
    ($selector): [{name: $release, minimumUpgradeAge: "0s"}]
  }}}' > test-channel.json
cat test-channel.json
```

For an existing cluster, edit `test-channel.json` before continuing: add its
current release after the candidate in `spec.releases[VERSION_SELECTOR]`, retaining
its existing minimum age. Review the edited request, then create the channel:

```sh
curl -i -H 'Content-Type: application/json' \
  --data-binary @test-channel.json \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels"
```

Expect HTTP 201. Set `CHANNEL_NAME` to this test channel and `CLUSTER_NAME` to
the test cluster. Follow [change a cluster's release selection](#change-a-clusters-release-selection),
then [verify the rollout](#verify-the-rollout). Keep other clusters on their existing channels.

After validation, set `CHANNEL_NAME` to the intended rollout channel and follow
[update the channel](#update-the-channel) to make the same release available there.
Record the tested versions, release, cluster and outcome. Distinguish initial
provisioning from upgrades or node replacement; success in one does not establish
the others.

## Administrator-assisted checks

Use these checks for troubleshooting or independent release qualification when
API status is insufficient. The administrator needs `kubectl` and read access to
the relevant datastore resources and workload nodes.

Set `TENANT_NAMESPACE` to the namespace holding the cluster's Cluster Service
resource. Confirm the active context points to the datastore, then inspect its
stored generation, conditions and events:

```sh
kubectl config current-context
kubectl -n "${TENANT_NAMESPACE:?}" get nvkeclusters.service.nvke.nvidia.com "${CLUSTER_NAME:?}" -o json
kubectl -n "${TENANT_NAMESPACE:?}" describe nvkeclusters.service.nvke.nvidia.com "${CLUSTER_NAME:?}"
```

Relevant conditions should have `observedGeneration` equal to
`metadata.generation`. Channel edits do not change the cluster's generation,
so also check the resolved release.

Follow the [workload cluster access procedure](../../../docs/workload-clusters/access.md)
to obtain a kubeconfig and arrange a route or approved proxy to its API. Set
`WORKLOAD_KUBECONFIG` to that file and inspect nodes:

```sh
kubectl --kubeconfig "${WORKLOAD_KUBECONFIG:?}" get nodes -o wide
```

All expected nodes should be present and `Ready`, with control-plane versions,
worker versions and node counts matching the requested configuration. For a new
image or initialization, verify the build identifier and expected initialization results
on newly provisioned or replaced nodes, and record their node or Machine
identities. Existing Ready nodes alone do not show that the candidate has booted.

Record provider health separately: a ready Kubernetes node can coexist with an
infrastructure health condition that still reports provisioning.

Created with AI assistance
