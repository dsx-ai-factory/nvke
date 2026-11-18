# Operator release workflow

An operator publishes images, releases, profiles, classes and channels through the
operator API. Tenants select those offerings through the tenant API; the manager
persists their resolved choices and reconciles them through the NVKE controller.
An accepted API request is not evidence that provisioning has completed.

## Before starting

Use a development environment agreed with its owner. Record the chart version,
manager/API image digests, datastore context and tenant namespace. Use matching
schemas and binaries; the current source tree can be ahead of the installed chart.
Older packages use the internal kind `NodePoolRelease` where current source uses
`NodeProfile`, and may report `resolvedRelease` instead of `currentRelease`.
Both expose worker offerings at `/v1/node-profiles`.

The installation needs the [Helm prerequisites](helm-installation.md), working
registry credentials, a running manager with a renewing Lease, and compatible
NVKE controller and infrastructure-provider versions. The tenant namespace must
exist and contain the infrastructure-template ConfigMap named by the manager.
Confirm the provider accepts the image's boot/PXE contract before requesting machines.

Agree these inputs with the environment and release owners:

| Input | Required content |
| --- | --- |
| `machine-image.json` | Public metadata/spec for an immutable image and its boot contract. |
| `cluster-release.json` | Kubernetes version, image reference and initialization. |
| `node-profile.json` | Worker image/initialization and a unique profile/tag pair. |
| `control-plane-class.json` | Supported instance types, replicas and rollout policy. |
| `release-channel.json` | Version selectors mapped to compatible releases and minimum upgrade ages. |
| `cluster-smoke.json` | A small tenant request using those offerings and approved network/capacity inputs. |

Use the public JSON shape, `metadata` plus `spec`, rather than a complete Kubernetes
manifest. Refer to the [resource types](../apis/v1) and the API contract from the
installed revision. Give test resources distinct names. Release inputs must name
real site artifacts; the [samples](../config/samples) are examples, not site defaults.

The operator needs catalog-write permission; the tenant needs access to its own
namespace and read-only catalog views. Kubernetes permission to port-forward is
separate. Configure caller authentication through the accepted installation
contract. The operator API remains internal. Keep local access options in
`operator-access.curl` and `tenant-access.curl`; do not commit credential-bearing
files or include tokens in captured evidence. The commands below assume those
files have been prepared for the approved test identities.

For example, this channel publishes an already-reviewed `smoke-release` under an
illustrative version selector. Use the actual release version and agreed upgrade
age in the test inputs:

```json
{
  "metadata": {"name": "smoke-channel"},
  "spec": {
    "releases": {
      "1.36": [{"name": "smoke-release", "minimumUpgradeAge": "0s"}]
    }
  }
}
```

## Reach the internal APIs

The following commands use the default release and Service names. Replace
`management-dev` with the explicit datastore/runtime context for the installation.
Run each port-forward in its own terminal and keep it open:

```sh
kubectl --context=management-dev -n cluster-service-system port-forward --address=127.0.0.1 service/cluster-service-operator-api 18081:8080
```

```sh
kubectl --context=management-dev -n cluster-service-system port-forward --address=127.0.0.1 service/cluster-service-tenant-api 18080:8080
```

```sh
curl --fail-with-body --config operator-access.curl http://127.0.0.1:18081/readyz
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/readyz
```

Readiness checks datastore discovery, not every permission or backend operation.
Port-forward testing also does not validate Gateway routing, DNS or TLS. A trusted
internal test without caller authentication is partial evidence; authenticated
qualification must use the agreed real identities.

## Publish and inspect the catalog

Publish reviewed inputs in dependency order. If an object already exists, read it
and compare its content instead of retrying the POST or replacing it blindly.

```sh
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @machine-image.json http://127.0.0.1:18081/v1/machine-images
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @cluster-release.json http://127.0.0.1:18081/v1/cluster-releases
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @node-profile.json http://127.0.0.1:18081/v1/node-profiles
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @control-plane-class.json http://127.0.0.1:18081/v1/control-plane-classes
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @release-channel.json http://127.0.0.1:18081/v1/cluster-release-channels
```

Expect successful create responses, then verify what the tenant can select:

```sh
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/control-plane-classes
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/cluster-release-channels
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/node-profiles
```

Offerings can be filtered by lifecycle and dependency availability. A missing
tenant view does not by itself mean the operator's POST failed.

To change an existing channel, GET the current object and ETag first. Prepare
`reviewed-channel.json` with the approved replacement `spec.releases`; preserve
the returned metadata and other spec fields:

```sh
curl --fail-with-body --config operator-access.curl -D channel.headers -o channel.json http://127.0.0.1:18081/v1/cluster-release-channels/smoke-channel
jq '.spec.releases = input.spec.releases' channel.json reviewed-channel.json > updated-channel.json
etag=$(awk 'tolower($1) == "etag:" {sub("\r$", ""); print $2}' channel.headers)
curl --fail-with-body --config operator-access.curl -X PUT -H "If-Match: $etag" -H 'Content-Type: application/json' --data-binary @updated-channel.json http://127.0.0.1:18081/v1/cluster-release-channels/smoke-channel
```

Use the actual channel name in both URLs. A stale ETag requires a fresh read and
review of concurrent changes. Publish new immutable images/releases/profile
identities rather than changing their immutable content.

## Request a cluster and verify reconciliation

Only submit the agreed request after template, catalog and provider prerequisites
are ready. The sample name below is `cluster-service-smoke`:

```sh
curl --fail-with-body --config tenant-access.curl -H 'Content-Type: application/json' --data-binary @cluster-smoke.json http://127.0.0.1:18080/v1/clusters
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/clusters/cluster-service-smoke
kubectl --context=management-dev -n nvke-service-smoke get nvkeclusters.service.nvke.nvidia.com cluster-service-smoke -o json
kubectl --context=management-dev -n cluster-service-system logs deployment/cluster-service-controller-manager --since=10m
```

Verify the stored request and current-generation conditions, resolved release
identity, backend resource/workflow correlation and actual cluster/worker state.
Observe progress through completion; API success, `Synced=True`, or a single Ready
condition alone is insufficient evidence of ready machines. Compare reported
worker counts with the backend and actual workers.

For a release change, read the cluster and ETag, review a full updated public
request, then PUT it to the named cluster endpoint with `If-Match`, as for the
channel update. Moving a channel forward does not bypass minimum upgrade ages or
guarantee an existing cluster adopts it immediately.

## Failures, recovery and cleanup

| Observation | Next check |
| --- | --- |
| Chart source fails to load | Application source URL/version and Argo repository access. |
| `ImagePullBackOff` | Namespace-local image-pull Secret delivery and the Pod's reference. |
| API returns 401/403 | Test identity, audience and authorization contract; do not disable checks to pass the test. |
| API readiness fails or datastore operations are forbidden | Datastore connection and the workload's own RBAC. |
| Request persists but reconciliation fails | Current-generation condition reason/message, tenant template location, catalog dependencies and backend/provider logs. |
| Release cannot update or delete | ETag conflicts, immutable fields, references and lifecycle protections in the installed version. |

Agree one failure case using isolated test input, such as an invalid profile
selection, and record the actionable API/condition error plus unchanged existing
workers. Do not remove shared credentials to manufacture a failure.

Recover by correcting the input or publishing a compatible new release. A channel
edit is not a guaranteed downgrade mechanism. For packaging failures use the
[installation upgrade and retention guidance](helm-installation.md); coordinate
provider/workload recovery with the owning operator.

Delete only agreed test resources through the API, using a fresh GET/ETag and
`If-Match` for DELETE. Wait for backend cleanup before removing dependent catalog
records. Keep shared CRDs, registry credentials, templates and namespaces.

Record the package/image references, test identity and tenant scope (no tokens),
resource UIDs/generations, API responses, backend correlation, failure outcome and
cleanup result. A human operator must repeat this procedure against the validated
environment before it is considered verified.

Created with AI assistance
