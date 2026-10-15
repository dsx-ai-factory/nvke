#!/bin/sh
set -eu
if ! temporal workflow execute \
  --address "$E2E_TEMPORAL_ADDRESS" \
  --namespace "$E2E_TEMPORAL_NAMESPACE" \
  --task-queue "$E2E_TEMPORAL_TASK_QUEUE" \
  --type RotateEtcdEncryptionKey \
  --workflow-id "$E2E_WORKFLOW_ID" \
  --input "{\"clusterID\":\"$E2E_CLUSTER_ID\"}" \
  --execution-timeout 15m \
  --run-timeout 15m \
  >/tmp/temporal-output 2>&1; then
  cat /tmp/temporal-output > /dev/termination-log
  exit 1
fi
printf 'workflow %s completed' "$E2E_WORKFLOW_ID" > /dev/termination-log
