# Operator release workflow

Operators publish releases that tenants select through the Cluster Service APIs.
This runbook covers the catalog, release selection and cluster verification.
For container images and Helm charts, see [package publication](artifact-publication.md).

## Prepare the installation and inputs

The examples use one cluster and default Helm names. Replace `management-dev`,
`nvke-service-smoke` and resource names with the agreed context, tenant and test resources.

Before starting, confirm:

- Manager, state and both APIs are installed with compatible, pinned packages.
  See [Helm installation](helm-installation.md) for values, credentials and RBAC.
- The manager is ready and renewing its leader-election Lease. The NVKE controller
  is reachable, and infrastructure providers support the image and boot scripts.
- The tenant namespace contains the manager's infrastructure-template ConfigMap.
  The request uses approved network settings and available capacity.
- The API workloads have their [operator](../config/dev/operator-api-rbac.yaml)
  and [tenant](../config/dev/tenant-api-rbac.yaml) datastore permissions. Callers
  use the agreed authentication setup; keep the operator API internal.
  Your Kubernetes identity also needs port-forwarding permission.

Prepare public JSON inputs using the schema from the installed version:

| File | Content |
| --- | --- |
| `machine-image.json` | Machine image and boot settings. |
| `cluster-release.json` | Kubernetes version, image reference and initialization. |
| `node-profile.json` | Worker image, initialization and unique profile/tag pair. |
| `control-plane-class.json` | Instance types, replicas and rollout settings. |
| `release-channel.json` | Version selectors, release references and minimum upgrade ages. |
| `cluster-smoke.json` | A small cluster request selecting the approved offerings. |

Create inputs contain `metadata.name` and `spec`; see the [resource types](../apis/v1)
and [samples](../config/samples). This channel selects an existing `smoke-release`:

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

Keep client credentials and options in local `operator-access.curl` and
`tenant-access.curl` files using the [curl configuration format](https://curl.se/docs/manpage.html#-K).
Protect these files and exclude them from Git and test evidence.

## Connect to the internal APIs

Run each port-forward in a separate terminal:

```sh
kubectl --context=management-dev -n cluster-service-system port-forward --address=127.0.0.1 service/cluster-service-operator-api 18081:8080
```

```sh
kubectl --context=management-dev -n cluster-service-system port-forward --address=127.0.0.1 service/cluster-service-tenant-api 18080:8080
```

```sh
curl --fail-with-body -i --config operator-access.curl http://127.0.0.1:18081/readyz
curl --fail-with-body -i --config tenant-access.curl http://127.0.0.1:18080/readyz
```

Expect HTTP 200. This checks API access and datastore discovery. Verify external
routing, DNS and TLS separately.

## Publish a release

Publish in dependency order. For an existing name, GET and compare the object first.

```sh
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @machine-image.json http://127.0.0.1:18081/v1/machine-images
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @cluster-release.json http://127.0.0.1:18081/v1/cluster-releases
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @node-profile.json http://127.0.0.1:18081/v1/node-profiles
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @control-plane-class.json http://127.0.0.1:18081/v1/control-plane-classes
curl --fail-with-body --config operator-access.curl -H 'Content-Type: application/json' --data-binary @release-channel.json http://127.0.0.1:18081/v1/cluster-release-channels
```

Expect HTTP 201 and the created object for each POST. Check tenant discovery:

```sh
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/control-plane-classes
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/cluster-release-channels
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/node-profiles
```

Expect HTTP 200. Hidden entries or unavailable dependencies can filter the tenant
view. Use new names for immutable image or release content.

## Update a channel

GET the channel and ETag (its version). Put the approved replacement `spec.releases`
in `reviewed-channel.json`. Preserve other spec fields and writable metadata:

```sh
curl --fail-with-body --config operator-access.curl -D channel.headers -o channel.json http://127.0.0.1:18081/v1/cluster-release-channels/smoke-channel
jq '{metadata: (.metadata | {name, uid}), spec} | .spec.releases = input.spec.releases' channel.json reviewed-channel.json > updated-channel.json
etag=$(awk 'tolower($1) == "etag:" {sub("\r$", ""); print $2}' channel.headers)
printf '%s\n' "$etag"
```

Stop if GET failed or no ETag was returned. Review `updated-channel.json`, then submit:

```sh
curl --fail-with-body --config operator-access.curl -X PUT -H "If-Match: $etag" -H 'Content-Type: application/json' --data-binary @updated-channel.json http://127.0.0.1:18081/v1/cluster-release-channels/smoke-channel
```

Expect HTTP 200. For a stale ETag (HTTP 412), read and review again. PUT accepts
`metadata.name`, optional `metadata.uid` and `spec`; omit timestamps and status.
Channel changes remain subject to minimum upgrade ages and existing selections.

## Apply the selection and verify the cluster

Submit the approved request, then inspect API and stored status:

```sh
curl --fail-with-body --config tenant-access.curl -H 'Content-Type: application/json' --data-binary @cluster-smoke.json http://127.0.0.1:18080/v1/clusters
curl --fail-with-body --config tenant-access.curl http://127.0.0.1:18080/v1/clusters/cluster-service-smoke
kubectl --context=management-dev -n nvke-service-smoke get nvkeclusters.service.nvke.nvidia.com cluster-service-smoke -o json
kubectl --context=management-dev -n cluster-service-system logs deployment/cluster-service-controller-manager --since=10m
```

Expect HTTP 201 for POST and 200 for GET. Follow the request until:

- Status reflects the latest requested generation and intended release.
- The matching cluster/workflow exists in the NVKE controller without errors.
- Actual cluster and worker readiness agree with reported progress and counts.

An accepted request or `Synced=True` alone does not prove provisioning completed.
For an existing cluster, GET it and its ETag, prepare the approved spec with only
writable metadata, and PUT to its named endpoint using `If-Match`, as above.
Keep the existing cluster name and UID.

## Diagnose failures and clean up

| Symptom | Check |
| --- | --- |
| Chart pull fails | Argo source URL/version and repository credentials. |
| `ImagePullBackOff` | Namespace-local pull Secret and the Pod's reference. |
| HTTP 401/403 | Caller identity, audience and permissions. |
| API unavailable or datastore forbidden | Datastore connection and workload RBAC. |
| Request stored, provisioning incomplete | Condition messages, template location, catalog references and backend/provider logs. |
| Update/delete rejected | ETag, immutable fields and references to the resource. |

Exercise one agreed failure on isolated input, such as an invalid profile. Record
the error and verify existing workers remain unchanged. Correct the input or
publish a compatible new release; a channel edit is not a guaranteed downgrade.
For installation problems, follow [upgrade and state-retention guidance](helm-installation.md#crd-ownership-upgrades-and-retention).

To remove the test cluster, obtain a fresh ETag, then DELETE:

```sh
curl --fail-with-body --config tenant-access.curl -D cluster.headers -o cluster.json http://127.0.0.1:18080/v1/clusters/cluster-service-smoke
etag=$(awk 'tolower($1) == "etag:" {sub("\r$", ""); print $2}' cluster.headers)
```

Stop if GET failed or no ETag was returned, then run:

```sh
curl --fail-with-body -i --config tenant-access.curl -X DELETE -H "If-Match: $etag" http://127.0.0.1:18080/v1/clusters/cluster-service-smoke
```

Expect HTTP 202. Wait for backend cleanup before removing test-owned catalog
entries; retain shared schemas, credentials and namespaces.

Record package and image references, identity and namespace, request UID and
generation, API results, backend outcome, failure check and cleanup.

Created with AI assistance
