# Configure a persistent exporter queue

The watchdog and the gateways queue telemetry in memory by default, so a
Collector that restarts loses what it had queued. NVKE telemetry pipeline (NKTP) does not configure a
persistent queue. This guide documents OpenTelemetry Collector configuration,
set through the chart's values, that keeps each exporter's
[queue](https://github.com/open-telemetry/opentelemetry-collector/blob/v0.162.0/exporter/exporterhelper/README.md#persistent-queue)
on a PersistentVolume with the
[`file_storage` extension](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/v0.162.0/extension/storage/filestorage).
The default Collector image includes the extension.

The queue holds data only while a Collector cannot reach its backend. Storing
it on disk does not change retries: a batch that still fails after
`retry_on_failure.max_elapsed_time`, 30 minutes by default, is dropped. Raise it
on the `otlp` exporter to keep data through longer outages.

## Prerequisites

A StorageClass that can provision volumes. Check that the cluster has a default
one:

```console
kubectl get storageclass
```

Without a default StorageClass, the volume claims stay `Pending` and so do the
Pods. Set `storageClassName` in each claim template to use a class that is not
the default.

## Configure the queue

From the repository root, edit the release name and namespace below to match
your installation, then install or upgrade with the
[values example](https://github.com/dsx-ai-factory/nvke/blob/main/docs/nktp/examples/persistent-queue.values.yaml), which configures
the watchdog and all three gateways:

```console
NKTP_RELEASE=nktp
NKTP_NAMESPACE=nktp
make nktp-chart-deps
helm upgrade --install "$NKTP_RELEASE" ./nktp/deploy/charts/nktp --namespace "$NKTP_NAMESPACE" \
  --values docs/nktp/examples/persistent-queue.values.yaml
```

For each Collector it adds a claim template, mounts the volume, makes it
writable by the Collector's user, and points the exporter's queue at it. For
the watchdog:

```yaml
watchdog:
  kubernetes:
    podSecurityContext:
      fsGroup: 10001
      fsGroupChangePolicy: OnRootMismatch
    volumeClaimTemplates:
      - metadata:
          name: queue
        spec:
          accessModes: [ReadWriteOnce]
          # storageClassName: <class>
          resources:
            requests:
              storage: 3Gi
    persistentVolumeClaimRetentionPolicy:
      whenDeleted: Retain
      whenScaled: Retain
    volumeMounts:
      - name: internal-tls-server
        mountPath: /etc/nktp/internal-tls/server
        readOnly: true
      - name: queue
        mountPath: /var/lib/otelcol/queue
  otelcol:
    config:
      extensions:
        file_storage/queue:
          directory: /var/lib/otelcol/queue
      exporters:
        otlp:
          sending_queue:
            storage: file_storage/queue
            queue_size: 536870912
      service:
        extensions: [health_check, file_storage/queue]
```

Volume mounts and `service.extensions` are lists, which Helm replaces, so the
example restates the chart's internal TLS mount and `health_check`. Merge the
lists by hand when combining it with another example, such as the one in
[Secure gateway export](secure-gateway-export.md). Helm merges
`sending_queue`, so its other default settings stay in place.

## Size the volumes

`queue_size` counts serialized bytes and bounds each queue. The chart's
defaults are 8MiB per signal for the watchdog and about 0.48 GiB for each
gateway. The example sets 512MiB for every queue, which is half of the space it
gives each queue.

Keep `queue_size` at no more than half of the space for each queue. A full
512MiB queue took 530MiB on disk, and the file grew to 635MiB while the queue
drained. The file does not shrink afterwards. When the volume fills, queue
writes fail with `no space left on device`. In a test with `queue_size` equal
to the volume, a restarted Collector took about six minutes to drain, lost the
batches it had in flight, and failed to delete some sent items, which can be
sent again after a later restart. Each gateway has one queue and claims 1Gi. The
watchdog keeps a separate queue for each of its three signals and claims 3Gi.
Raise `queue_size` and the claim together to ride out longer backend outages.

Request at least the provider's minimum volume size. OCI block volumes have a
minimum of 50GB.

Each replica that has run keeps its own claim. The watchdog autoscales between
2 and 16 replicas and each gateway between 2 and 128, so at their maximum that
is up to 400 volumes, or 20TB at the OCI minimum. Lower
`autoscaler.maxReplicas` to bound it. With `whenScaled: Retain`, telemetry
queued on a removed replica is sent when that replica scales back out. With
`Delete`, it is discarded.

## Change the claim templates

Kubernetes does not allow changing the volume claim templates of a
StatefulSet. When they are added or changed, the OpenTelemetry Operator deletes
the StatefulSet and creates it again, so every replica of that Collector
restarts at once. Senders retry while it is unavailable. Make this change during
a quiet period. `whenDeleted: Retain` keeps the claims through this, and the new
replicas reuse them as they are. With `Delete`, the queued data is deleted.

## Delete the claims

Retained claims stay after NKTP is uninstalled. Edit the namespace below, then
delete them when they are no longer needed:

```console
NKTP_NAMESPACE=nktp
for collector in watchdog gateway-logs gateway-metrics gateway-traces; do
  kubectl delete pvc --namespace "$NKTP_NAMESPACE" \
    --selector "app.kubernetes.io/instance=$NKTP_NAMESPACE.nktp-$collector"
done
```

## Verify

The `otelcol_exporter_queue_size` metric of each Collector shows queued data.
Each replica has a bound claim named after its Pod. Edit the namespace below,
then list them:

```console
NKTP_NAMESPACE=nktp
kubectl get pvc --namespace "$NKTP_NAMESPACE" | grep -E "nktp-(watchdog|gateway)-"
```
