# Operator release workflow

Operators use the Cluster Service APIs to publish releases and make them available
through channels. This runbook covers the routine release workflow:

1. Publish a new release or select an existing one.
2. Update the intended channel so eligible clusters following it adopt the release.
3. Monitor the resulting cluster operations and verify completion.
4. Diagnose failures and retire unused releases when appropriate.

Publishing adds a catalog record. Updating a channel changes the releases available
to clusters following it, according to their version selection and eligibility
rules. A cluster can also be moved to another channel or version selection.

For container images and Helm charts, see [package publication](artifact-publication.md).
For validation on one cluster, see [test a release before wider rollout](#test-a-release-before-wider-rollout).

## Before starting

You need `curl`, `jq` and `kubectl`, access to the Cluster Service APIs and
datastore, and a kubeconfig for the workload cluster. Confirm that Cluster Service
meets the [installation prerequisites](helm-installation.md) and can reach the
NVKE controller.

Use the site's tenant API endpoint and its approved internal
endpoint for the operator API. Both refer to Cluster Service. Add the authentication
options required by your deployment to each curl command.

| Variable | Value |
| --- | --- |
| `TENANT_API_URL` | Published tenant-facing Cluster Service base URL, without a trailing slash or `/v1`. |
| `OPERATOR_API_URL` | Approved internal operator-facing Cluster Service base URL, without a trailing slash or `/v1`. |
| `TENANT_NAMESPACE` | Namespace holding the tenant's Cluster Service resources. |
| `CLUSTER_NAME` | Cluster to inspect or change, identified within `TENANT_NAMESPACE`. |

Confirm the active Kubernetes context points to the cluster holding Cluster Service state:

```sh
kubectl config current-context
```

Use an operator identity with catalog read/write access and a tenant identity
with read/update access to the target cluster. The `kubectl` checks require read
access to Cluster Service state and workload nodes. Keep credentials out of Git
and captured results.

## Select the release inputs

Choose an existing machine image and identify the channel to update:

```sh
curl -i "${OPERATOR_API_URL:?}/v1/machine-images"
curl -i "${OPERATOR_API_URL:?}/v1/cluster-release-channels"
```

For a new release, set its name, image, Kubernetes version and approved cloud-init
template from the release artifacts. The selector identifies the version offered
through the channel; minimum upgrade age controls when a new release is eligible.

| Variable | Value |
| --- | --- |
| `RELEASE_NAME` | New or existing release to make available. |
| `MACHINE_IMAGE_NAME` | Existing machine image from the catalog. |
| `KUBERNETES_VERSION` | Exact Kubernetes version the release should boot. |
| `USER_DATA_TEMPLATE` | Path to the approved cloud-init template from the release artifacts. |
| `CHANNEL_NAME` | Existing channel to update or select for a cluster. |
| `VERSION_SELECTOR` | Channel selector: `major.minor` allows patch upgrades; `major.minor.patch` selects an exact Kubernetes version. |
| `MINIMUM_UPGRADE_AGE` | Minimum release age before existing clusters may adopt it, such as `24h`. |

Use the schema from the installed version. The [release definition](../apis/v1/clusterrelease_types.go)
and [initialization fields](../apis/v1/common_types.go) describe optional settings,
including additional bootstrap files and commands.

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
  --rawfile userData "${USER_DATA_TEMPLATE:?}" \
  '{metadata: {name: $name}, spec: {
    kubernetes: {version: $version},
    machineImageRef: {name: $image},
    machineInit: {userDataTemplate: $userData}
  }}' > release.json
cat release.json
```

Include any `machineInit.infra` files and commands required by the image in
`release.json`, then publish the reviewed request:

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

Replace that selector's candidate list with the new release, preserving other
selectors and settings. Keep only writable metadata from the GET response:

```sh
jq --arg selector "${VERSION_SELECTOR:?}" \
  --arg release "${RELEASE_NAME:?}" \
  --arg age "${MINIMUM_UPGRADE_AGE:?}" \
  '{metadata: (.metadata | {name, uid}), spec}
   | .spec.releases[$selector] = [{name: $release, minimumUpgradeAge: $age}]' \
  channel.json > channel-update.json
cat channel-update.json
```

Submit the reviewed update:

```sh
curl -i -X PUT \
  -H "If-Match: ${CHANNEL_ETAG:?}" -H 'Content-Type: application/json' \
  --data-binary @channel-update.json \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels/${CHANNEL_NAME:?}"
```

Expect HTTP 200. A stale ETag returns HTTP 412: read the resource and review the
change again. Updates accept `metadata.name`, optional `metadata.uid` and `spec`;
timestamps and status are read-only.

Monitor adopting clusters using the [rollout checks](#verify-the-rollout).
Adoption depends on each cluster's selection and the release's eligibility; a
channel update does not mean every cluster upgrades immediately.

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

Worker releases are selected separately through [node profiles](../apis/v1/nodeprofile_types.go).
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

For each affected cluster, set `CLUSTER_NAME` and `TENANT_NAMESPACE` to its values.
Use the context for the Cluster Service datastore:

```sh
curl -i "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
kubectl -n "${TENANT_NAMESPACE:?}" get nvkeclusters.service.nvke.nvidia.com "${CLUSTER_NAME:?}" -o json
```

Check the stored resource and the workload cluster together:

| Check | Expected result |
| --- | --- |
| Condition freshness | Each relevant condition's `observedGeneration` matches `metadata.generation`. Channel edits do not change the cluster's generation, so also check the resolved release. |
| `status.currentRelease` | Names the intended control-plane release and Kubernetes version. This records backend configuration, not completed node replacement. |
| `status.conditions` | Current `Synced`, `ControlPlaneReady` and `Ready` observations are `True`; inspect their reasons and messages if not. |
| Backend operation | `NVKEControllerProvisioned=True` and `NVKEControllerProvisioning=False`. For requested upgrades or recycling, also confirm those operations finish and inspect any `NVKEControllerUpdateFailed` error. |

Use the [workload cluster access procedure](../../docs/workload-clusters/access.md)
to obtain its kubeconfig and arrange a route or approved proxy to the workload
API. Access to the management cluster alone does not provide that connection.
Set `WORKLOAD_KUBECONFIG` to the resulting file and inspect nodes:

```sh
kubectl --kubeconfig "${WORKLOAD_KUBECONFIG:?}" get nodes -o wide
```

All expected nodes must be present and `Ready`. Control-plane nodes must run the
selected Kubernetes version; worker nodes must match their selected node profiles.
Compare actual node counts with the requested pools. API success, `currentRelease`
or `Ready=True` alone does not establish that replacement or scaling has finished.
Record provider health separately: a ready Kubernetes node can coexist with an
infrastructure health condition that still reports provisioning.

## Diagnose failures

Inspect the resource's conditions and events:

```sh
kubectl -n "${TENANT_NAMESPACE:?}" describe nvkeclusters.service.nvke.nvidia.com "${CLUSTER_NAME:?}"
```

| Symptom | Next action |
| --- | --- |
| API unavailable or HTTP 401/403 | Check the endpoint and workload readiness; confirm the site's caller credentials and permissions before retrying. |
| HTTP 412 on update | Fetch the resource again, review changes and rebuild the request with its new ETag. |
| Release not selected | Check the channel/version selection, minimum upgrade age and release lifecycle fields. Correct those inputs before retrying. |
| Nodes fail to initialize or become Ready | Inspect node bootstrap and agent logs. Check network access, workload identity and compatibility of the image, initialization and CNI packages. |
| `ClusterReconciled=False` or `NodePoolsReconciled=False` | Read the condition message. Check the referenced catalog resources and tenant infrastructure template; correct the failing input. |
| `NVKEControllerUpdateFailed=True`, stalled replacement or nodes not Ready | Capture the condition messages, backend cluster ID (`status.id`) and node state. Involve the backend/provider owner before making another version change. |

For a bad release, stop promoting it to other channels while investigating. A
channel edit or Helm rollback does not undo a workload cluster upgrade. Follow
the [workload upgrade guidance](../../docs/workload-clusters/upgrade.md) for supported
control-plane and worker operations; recovery from a partially completed upgrade
needs the backend/provider owner's assessment. For package or datastore problems,
see [upgrade and state retention](helm-installation.md#crd-ownership-upgrades-and-retention).

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

Create the reviewed channel:

```sh
curl -i -H 'Content-Type: application/json' \
  --data-binary @test-channel.json \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels"
```

Expect HTTP 201. Set `CHANNEL_NAME` to this test channel and `CLUSTER_NAME` and
`TENANT_NAMESPACE` to the test cluster. Follow [change a cluster's release selection](#change-a-clusters-release-selection),
then [verify the rollout](#verify-the-rollout). Keep other clusters on their existing channels.

After validation, set `CHANNEL_NAME` to the intended rollout channel and follow
[update the channel](#update-the-channel) to make the same release available there.
Record the tested versions, release, cluster and outcome. Distinguish initial
provisioning from upgrades or node replacement; success in one does not establish
the others.

Created with AI assistance
