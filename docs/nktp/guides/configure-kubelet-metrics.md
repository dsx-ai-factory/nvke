# Configure kubelet metrics

Each DaemonSet collector scrapes the kubelet on its own node over HTTPS every
30 seconds. The collector connects directly to the node's host IP on port 10250.
These scrapes do not use a ServiceMonitor or the Target Allocator. The collector
authenticates with its projected service-account token and has read-only
`nodes/metrics` permission.

The scrape jobs are in the `prometheus/kubelet` receiver of
`daemonset.otelcol.config` in `nktp/deploy/charts/nktp/values.yaml`. By default there are two jobs:

- `kubelet` scrapes `/metrics`.
- `kubelet-cadvisor` scrapes `/metrics/cadvisor`. Its `metric_relabel_configs`
  are [copied from kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/blob/4713c624771a48e99f16dec9eb55ec8204f06830/charts/kube-prometheus-stack/values.yaml#L1952).
  They drop samples that are less useful, always zero, overlapping with
  kube-state-metrics, from host network interfaces, or from cgroups with no pod.

The chart renders this configuration exactly as written. To change the scrapes,
change the configuration. Helm replaces a list in your values file as a whole,
so copy the full `scrape_configs` list from `nktp/deploy/charts/nktp/values.yaml` into your values
file and change your copy.

## Change the scrapes

- To stop scraping cAdvisor, remove the `kubelet-cadvisor` job.
- To change the cAdvisor filters, edit the job's `metric_relabel_configs`.
- To scrape `/metrics/probes` or `/metrics/resource`, copy the `kubelet` job.
  Then give the copy a new `job_name`, such as `kubelet-probes`, and set both
  `metrics_path` and the `replacement` of the `metrics_path` relabel rule to the
  new path.

## Verify kubelet certificates

The certificate authorities that sign kubelet serving certificates, and the
subject alternative names in those certificates, vary between Kubernetes
distributions. For this reason, certificate verification is disabled by
default. Each kubelet certificate must cover the node IP that the collector
uses.

To enable verification, first create a ConfigMap in the NVKE telemetry pipeline (NKTP) release namespace
that contains the kubelet-serving CA:

```console
kubectl create configmap kubelet-serving-ca \
  --namespace <release-namespace> \
  --from-file=ca.crt=/path/to/kubelet-serving-ca.crt
```

Then mount it into the collector. Add this volume to
`daemonset.kubernetes.volumes` and this mount to
`daemonset.kubernetes.volumeMounts`. These are lists, so copy the default
entries from `nktp/deploy/charts/nktp/values.yaml` and add these to them:

```yaml
daemonset:
  kubernetes:
    volumes:
      # ...default volumes...
      - name: kubelet-serving-ca
        configMap:
          name: kubelet-serving-ca
          # If the CA is under a key other than ca.crt, map it to ca.crt:
          # items:
          #   - key: <key>
          #     path: ca.crt
    volumeMounts:
      # ...default volume mounts...
      - name: kubelet-serving-ca
        mountPath: /etc/nktp/kubelet-serving-ca
        readOnly: true
```

Finally, set `tls_config` in each kubelet scrape job:

```yaml
tls_config:
  ca_file: /etc/nktp/kubelet-serving-ca/ca.crt
  insecure_skip_verify: false
```

## Use with kube-prometheus-stack

When kube-prometheus-stack is installed alongside NKTP, disable its kubelet
ServiceMonitor only after NKTP scrapes the kubelet endpoints that you need and
their consumers have moved. This prevents scraping kubelet `/metrics` twice:

```yaml
kubelet:
  serviceMonitor:
    enabled: false
```

As a best-effort fallback, the DaemonSet Target Allocator excludes
ServiceMonitors with the conventional `app` labels
`kube-prometheus-stack-kubelet` and `prometheus-operator-kubelet`. This prevents
it from assigning a kubelet scrape that another system manages. ServiceMonitor
names and labels are configurable, so this fallback does not find every kubelet
ServiceMonitor.
