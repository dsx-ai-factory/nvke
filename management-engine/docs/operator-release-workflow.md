# Operator release workflow

Operators publish releases that tenants select through the Cluster Service APIs.
This runbook publishes a release, makes it available through a channel and applies
it to an existing cluster. For container images and Helm charts, see
[package publication](artifact-publication.md).

## Before starting

Confirm the [installation prerequisites](helm-installation.md): compatible pinned
packages, working API and manager identities, a ready manager and a reachable
NVKE controller. The image, initialization and infrastructure-provider versions
must support the intended Kubernetes version and cluster operation.

Use the site's published Gateway URL for the tenant API and its approved internal
endpoint for the operator API. Both refer to Cluster Service. Add the authentication
options required by your deployment to each curl command.

Run the commands in Bash and set these variables for your environment. Use base URLs without a trailing
slash or `/v1`. The `${VAR:?}` checks stop a command if a required value is
unset or empty.

| Variable | Value |
| --- | --- |
| `TENANT_API_URL` | Published tenant-facing Cluster Service base URL. |
| `OPERATOR_API_URL` | Approved internal operator-facing Cluster Service base URL. |
| `TENANT_NAMESPACE` | Namespace holding the tenant's Cluster Service resources. |
| `CLUSTER_NAME` | Existing cluster to update. |

Confirm the active Kubernetes context points to the cluster holding Cluster Service state:

```sh
kubectl config current-context
```

The operator needs catalog-write access; the tenant needs access to its cluster.
The API workloads need their canonical [operator](../config/dev/operator-api-rbac.yaml)
and [tenant](../config/dev/tenant-api-rbac.yaml) datastore permissions. Keep credentials
out of Git and captured results.

## Select the release inputs

Choose an existing machine image and channel from the catalog:

```sh
curl -i \
  "${OPERATOR_API_URL:?}/v1/machine-images"
curl -i \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels"
```

Set the new release's name, image, Kubernetes version and approved cloud-init
template from the release artifacts. The selector identifies the version offered
through the channel; minimum upgrade age controls when a new release is eligible.

| Variable | Value |
| --- | --- |
| `RELEASE_NAME` | Name for the new release. |
| `MACHINE_IMAGE_NAME` | Existing machine image from the catalog. |
| `KUBERNETES_VERSION` | Exact Kubernetes version carried by the image. |
| `USER_DATA_TEMPLATE` | Path to the approved cloud-init template from the release artifacts. |
| `CHANNEL_NAME` | Existing channel to update. |
| `VERSION_SELECTOR` | Version selector offered through the channel. |
| `MINIMUM_UPGRADE_AGE` | Eligibility delay, such as `24h`. |

Use the schema from the installed version. The [release definition](../apis/v1/clusterrelease_types.go)
and [initialization fields](../apis/v1/common_types.go) describe optional settings,
including additional bootstrap files and commands.

## Publish the release

Build the request directly from those inputs, then inspect it:

```sh
release_body=$(jq -n \
  --arg name "${RELEASE_NAME:?}" \
  --arg image "${MACHINE_IMAGE_NAME:?}" \
  --arg version "${KUBERNETES_VERSION:?}" \
  --rawfile userData "${USER_DATA_TEMPLATE:?}" \
  '{metadata: {name: $name}, spec: {
    kubernetes: {version: $version},
    machineImageRef: {name: $image},
    machineInit: {userDataTemplate: $userData}
  }}')
printf '%s\n' "${release_body:?}" | jq .
```

After review, publish it:

```sh
curl -i \
  -H 'Content-Type: application/json' \
  --data-binary "${release_body:?}" \
  "${OPERATOR_API_URL:?}/v1/cluster-releases"
```

Expect HTTP 201 and the created release. If the name already exists, GET and compare
it first. Immutable image or initialization changes require a new release name.

## Update the channel

GET the existing channel and its ETag, which identifies the version being edited.
Continue only after HTTP 200 and a nonempty ETag:

```sh
curl \
  -w 'HTTP %{http_code}\n' \
  -D channel.headers -o channel.json \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels/${CHANNEL_NAME:?}"
etag=$(awk 'tolower($1) == "etag:" {sub("\r$", ""); print $2}' channel.headers)
printf 'ETag: %s\n' "${etag:?}"
```

Replace that selector's candidate list with the new release, preserving other
selectors and settings. Keep only writable metadata from the GET response:

```sh
channel_body=$(jq \
  --arg selector "${VERSION_SELECTOR:?}" \
  --arg release "${RELEASE_NAME:?}" \
  --arg age "${MINIMUM_UPGRADE_AGE:?}" \
  '{metadata: (.metadata | {name, uid}), spec}
   | .spec.releases[$selector] = [{name: $release, minimumUpgradeAge: $age}]' \
  channel.json)
printf '%s\n' "${channel_body:?}" | jq .
```

After review, submit the update:

```sh
curl -i \
  -X PUT -H "If-Match: ${etag:?}" -H 'Content-Type: application/json' \
  --data-binary "${channel_body:?}" \
  "${OPERATOR_API_URL:?}/v1/cluster-release-channels/${CHANNEL_NAME:?}"
```

Expect HTTP 200. A stale ETag returns HTTP 412: read the resource and review the
change again. Updates accept `metadata.name`, optional `metadata.uid` and `spec`;
timestamps and status are read-only.

## Select the release for a cluster

Changing a channel can affect clusters already following it. To select the channel
and version explicitly for an existing cluster, GET its current state and ETag:

```sh
curl \
  -w 'HTTP %{http_code}\n' \
  -D cluster.headers -o cluster.json \
  "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
etag=$(awk 'tolower($1) == "etag:" {sub("\r$", ""); print $2}' cluster.headers)
printf 'ETag: %s\n' "${etag:?}"
```

Expect HTTP 200 and an ETag before building the update:

```sh
cluster_body=$(jq \
  --arg channel "${CHANNEL_NAME:?}" --arg version "${VERSION_SELECTOR:?}" \
  '{metadata: (.metadata | {name, uid}), spec}
   | .spec.release.channel = {name: $channel, version: $version}' \
  cluster.json)
printf '%s\n' "${cluster_body:?}" | jq .
```

Review the complete spec, including the unchanged node pools, before submitting:

```sh
curl -i \
  -X PUT -H "If-Match: ${etag:?}" -H 'Content-Type: application/json' \
  --data-binary "${cluster_body:?}" \
  "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
```

Expect HTTP 200. The manager resolves the selection subject to channel eligibility
and minimum upgrade age. A successful API response means the request was stored.

Worker releases are selected separately through [node profiles](../apis/v1/nodeprofile_types.go).
Set `NODE_POOL_NAME` to the existing pool and `NODE_PROFILE` to its approved
`profile:tag`. GET the cluster and ETag again, then use this transformation in
place of the channel selection above. Submit it with the same PUT command:

```sh
cluster_body=$(jq \
  --arg pool "${NODE_POOL_NAME:?}" --arg profile "${NODE_PROFILE:?}" \
  'if any(.spec.nodePools[]?; .name == $pool) then
     {metadata: (.metadata | {name, uid}), spec}
     | (.spec.nodePools[] | select(.name == $pool) | .nodeProfile) = $profile
   else error("node pool not found") end' \
  cluster.json)
printf '%s\n' "${cluster_body:?}" | jq .
```

## Verify the rollout

```sh
curl -i \
  "${TENANT_API_URL:?}/v1/clusters/${CLUSTER_NAME:?}"
kubectl -n "${TENANT_NAMESPACE:?}" \
  get nvkeclusters.service.nvke.nvidia.com "${CLUSTER_NAME:?}" -o json
```

Verify that status reflects the latest requested generation and intended release,
then check the matching NVKE controller operation and actual node readiness.
Reported progress and worker counts should agree with the running cluster.
Use the deployment's [Kubernetes API access procedure](../../docs/tenant-clusters/kubernetes-api-access.md)
to inspect workload nodes. API success or `Synced=True` alone does not prove the
rollout completed.

## Diagnose failures and retire old releases

| Symptom | Check |
| --- | --- |
| API unavailable or HTTP 401/403 | Endpoint, caller credentials, permissions and workload readiness. |
| Update rejected | ETag, immutable fields and resource references. |
| Release not selected | Channel selector, minimum upgrade age and lifecycle settings. |
| Request stored, rollout incomplete | Condition messages, tenant template, catalog references and backend/provider logs. |

Correct the input or publish a compatible new release. A channel edit is not a
guaranteed downgrade. For installation problems, follow
[upgrade and state-retention guidance](helm-installation.md#crd-ownership-upgrades-and-retention).

Retain previous releases for the agreed recovery period. Once an old release is
unused and eligible for removal, set `RETIRED_RELEASE_NAME` and get a fresh ETag:

```sh
curl \
  -w 'HTTP %{http_code}\n' \
  -D retired-release.headers -o retired-release.json \
  "${OPERATOR_API_URL:?}/v1/cluster-releases/${RETIRED_RELEASE_NAME:?}"
etag=$(awk 'tolower($1) == "etag:" {sub("\r$", ""); print $2}' retired-release.headers)
printf 'ETag: %s\n' "${etag:?}"
```

After HTTP 200, confirm the release is unused, then delete it:

```sh
curl -i \
  -X DELETE -H "If-Match: ${etag:?}" \
  "${OPERATOR_API_URL:?}/v1/cluster-releases/${RETIRED_RELEASE_NAME:?}"
```

Expect HTTP 202. Remove stale channel references before removing a release; retain
shared images, schemas, credentials and namespaces still in use.

Record package and image references, cluster and release identities, API results,
observed outcome and any recovery actions. Keep the generated response files for
review according to the site's data-handling rules.

Created with AI assistance
