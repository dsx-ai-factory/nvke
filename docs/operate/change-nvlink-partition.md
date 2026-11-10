# Change a node pool's NVLink partition

This page describes how to set or replace the NVLink logical partition of an NVIDIA Infra Controller (NICo) node pool in an NVIDIA Kubernetes Engine (NVKE) workload cluster, and how to attach the pool's existing nodes to it without replacing them. You do this on a site with NICo NVLink partitioning, when a pool was created without a partition or with the wrong one.

A node pool update with `nico.nvLinkLogicalPartitionID` changes the pool's node configuration, as a release change does. NVKE records the new configuration and uses it for every node it creates for the pool from then on: scale-up, automatic provisioning, and the replacement of a deleted node. Nodes that already exist keep their partition until you replace them or update their NICo instances. The partition can be set or replaced, but not cleared.

## Before you begin

- You have completed [Configure an infrastructure provider: NICo](provider-nico.md) and [Manage node pools](../workload-clusters/node-pools.md), and you know the `<API_URL>` of the NVKE API and the `<NODE_POOL_ID>` of the pool.
- You have the ID of the NVLink logical partition, created in NICo for the cluster's VPC.
- You have `curl`, `jq` and a kubeconfig for the management cluster that can read Secrets in the `capnico-system` namespace.

## Change the partition

> **Warning:** Change the partition only when no upgrade or rollback of the NVKE controller is in progress. During one, a node can be created with the pool's new partition instead of the configuration it belongs to, and a retry of that node's creation can then fail.

1. Send the update to the pool.

   ```bash
   curl -sS --fail-with-body -X PATCH "<API_URL>/api/nodePools/<NODE_POOL_ID>" \
     -H 'Content-Type: application/json' \
     -d '{"nico": {"nvLinkLogicalPartitionID": "<NVLINK_LOGICAL_PARTITION_ID>"}}'
   ```

   If API authentication is on, add the `Authorization` header as described in [Manage node pools](../workload-clusters/node-pools.md). The response is the updated pool in JSON, with the new `nvLinkLogicalPartitionID` in `nico`. The all-zero UUID returns 400.

   If `curl` reports an error, do not continue until the pool has the new partition. A timeout or a dropped connection can come after the update is saved, so read the pool first:

   ```bash
   curl -sS "<API_URL>/api/nodePools/<NODE_POOL_ID>" | jq -r '.nico.nvLinkLogicalPartitionID'
   ```

   If the output is not the new partition, correct the request and send it again. Otherwise the nodes the pool creates from then on do not get the partition, even after you attach the existing nodes.

2. Decide what happens to the existing nodes. They keep their partition, and the pool's settings decide whether NVKE marks them for replacement:

   - With the default `recycleBudget` of 0, NVKE leaves them alone.
   - With `recycleBudget` above 0 and auto mode on, NVKE marks up to `recycleBudget` of them at a time with the condition `nvke.nvidia.com/NeedsRecycle`, as described in [Control automatic node replacement](../workload-clusters/node-pools.md#control-automatic-node-replacement). NVKE does not delete marked nodes. When you or a break-fix system such as NVSentinel deletes one, the pool replaces it with a node on the new partition.
   - With `maxSurge` above 0, the pool creates up to `maxSurge` extra nodes on the new partition while nodes with the earlier configuration remain.

   To keep the existing nodes and attach them to the new partition, continue with [Attach existing nodes to an NVLink partition without redeploying them](#attach-existing-nodes-to-an-nvlink-partition-without-redeploying-them). A node you attach this way still has the earlier configuration as far as NVKE is concerned, so auto mode can still mark it.

## Attach existing nodes to an NVLink partition without redeploying them

> **Note:** Follow this procedure when you need to change the NVLink partition of deployed nodes in place. When a worker node is recycled (terminated), its replacement takes the NVLink partition of the node pool's current configuration. The replacement therefore lacks the partition unless you updated the pool with `nico.nvLinkLogicalPartitionID` as shown above.

Update the NICo instance of each existing node. NICo:

- accepts the update only while the instance is `Ready`, so a node that is still provisioning returns 400 until it is;
- replaces all of the instance's NVLink interfaces with the ones you send, and keeps the ones that already match;
- rejects the update if the VPC has a default NVLink logical partition and the instance already has NVLink interfaces.

While NICo applies the change the instance is `Configuring`, and workloads on the node that use NVLink can be interrupted. Drain the node first if that matters.

Send one interface per GPU of the instance type, with `deviceInstance` from 0 to the GPU count minus one; a GB200 or GB300 compute tray has 4 GPUs.

1. Read the NICo API path from [the NICo credentials](provider-nico.md#manage-the-nico-credentials), write the `Authorization` header to a file only you can read, and build the request body from the partition and the GPU count. The header file keeps the token out of the command line of every `curl` below.

   ```bash
   NICO_API="$(kubectl -n capnico-system get secret nico-credentials \
     -o go-template='{{.data.endpoint | base64decode}}/v2/org/{{.data.orgID | base64decode}}/{{.data.apiName | base64decode}}')"
   NICO_AUTH="$(mktemp)"
   kubectl -n capnico-system get secret nico-credentials \
     -o go-template='Authorization: Bearer {{.data.token | base64decode}}' > "${NICO_AUTH}"
   PARTITION="<NVLINK_LOGICAL_PARTITION_ID>"
   GPUS=4
   BODY="$(jq -n --arg p "${PARTITION}" --argjson n "${GPUS}" \
     'if $n < 1 then error("GPUS must be at least 1")
      else {nvLinkInterfaces: [range(0; $n) | {nvLinkLogicalPartitionId: $p, deviceInstance: .}]} end')"
   ```

   If `jq` reports an error, set `GPUS` to the GPU count of the instance type and run the last command again. `mktemp` creates the header file readable only by you; delete it with `rm "${NICO_AUTH}"` when you are done.

2. Update one instance. The instance ID is the part of the NicoMachine's `spec.providerID` after `nico://`.

   ```bash
   curl -sS -X PATCH "${NICO_API}/instance/<INSTANCE_ID>" \
     -H @"${NICO_AUTH}" -H "Content-Type: application/json" -d "${BODY}"
   ```

3. To update every node of the pool, collect the pool's NicoMachines on the management cluster. Older controllers label them with `nke.nvidia.com/node-pool-id` instead of `nvke.nvidia.com/node-pool-id`; the filter accepts both.

   ```bash
   kubectl get nicomachines -A -o json \
     | jq --arg pool "<NODE_POOL_ID>" '[.items[]
         | select((.spec.labels["nvke.nvidia.com/node-pool-id"] // .spec.labels["nke.nvidia.com/node-pool-id"]) == $pool)]' > pool-machines.json
   ```

   Write the instance IDs to a file, and list the machines that have no instance yet.

   ```bash
   jq -r '.[] | .spec.providerID // empty | sub("^nico://"; "")' pool-machines.json > instances.txt
   jq -r '.[] | select(.spec.providerID == null) | "\(.metadata.namespace)/\(.metadata.name)"' pool-machines.json
   ```

   A machine in the second list is still creating its instance, with the partition of the configuration it belongs to. If that is the earlier configuration, repeat this procedure for it once it has a provider ID.

4. Update each instance that is not already on the partition. Nodes that the pool created after the update, such as surge nodes, already are. The loop prints `skip` for those, `read failed` with the response body for an instance it cannot read, the status code of each update, and the response body of any update that did not return 200.

   ```bash
   while read -r instance; do
     rm -f instance.json
     if ! curl -sS --fail-with-body -o instance.json "${NICO_API}/instance/${instance}" -H @"${NICO_AUTH}"; then
       echo "read failed ${instance}"; cat instance.json 2>/dev/null; echo; continue
     fi
     jq -e --arg p "${PARTITION}" --argjson n "${GPUS}" \
       '[.nvLinkInterfaces[]?.nvLinkLogicalPartitionId] | length == $n and all(. == $p)' instance.json > /dev/null
     case $? in
       0) echo "skip ${instance}"; continue ;;
       1) ;;
       *) echo "read failed ${instance}"; cat instance.json; echo; continue ;;
     esac
     code="$(curl -sS -o response.json -w '%{http_code}' -X PATCH "${NICO_API}/instance/${instance}" \
       -H @"${NICO_AUTH}" -H "Content-Type: application/json" -d "${BODY}")"
     echo "${code} ${instance}"
     if [ "${code}" != 200 ]; then cat response.json; echo; fi
   done < instances.txt
   ```

   Read the response body before you run the loop again for an instance. A server error or a timeout can succeed on a retry, and so can a 400 for an instance that was not yet `Ready`. A 400 because the VPC has a default NVLink logical partition fails again on every retry while the VPC keeps that partition. Every update moves the instance to `Configuring`, even one that changes no interface, so the loop skips instances that are already on the partition. Run the loop again for an instance it could not read.

## Verify

1. Check that the pool's current configuration carries the partition.

   ```bash
   NODE_SET_ID="$(curl -sS "<API_URL>/api/nodePools/<NODE_POOL_ID>" | jq -r '.currentNodeSetID')"
   curl -sS "<API_URL>/api/nodeSets/${NODE_SET_ID}" | jq -r '.nico.nvLinkLogicalPartitionID'
   ```

   The output is the partition ID you set. Add the `Authorization` header to both requests if API authentication is on.

2. If you attached existing nodes, check that each instance is back to `Ready` with every NVLink interface on the partition. A 200 from the update means NICo accepted it, not that it is applied.

   ```bash
   while read -r instance; do
     curl -sS "${NICO_API}/instance/${instance}" -H @"${NICO_AUTH}" \
       | jq -r --arg p "${PARTITION}" --argjson n "${GPUS}" \
         '"\(.id) \(.status) " + (if $n > 0 and ([.nvLinkInterfaces[]?.nvLinkLogicalPartitionId] | length == $n and all(. == $p)) then "attached" else "not attached" end)'
   done < instances.txt
   ```

   An instance that is still `Configuring` has not finished; check it again later. Update again any instance that is `Ready` but reports `not attached`.

## Next steps

- [Manage node pools](../workload-clusters/node-pools.md) to replace nodes or tune auto mode.
- [Troubleshoot](troubleshoot.md) if new nodes do not come up.
