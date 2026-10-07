# Enable host process metrics

The node Collector does not collect per-process metrics by default. Node
exporter provides host metrics, and the kubelet cAdvisor scrape provides
per-container resource use. This guide enables the `process` scraper of the
`hostmetrics` receiver on the node Collector, for CPU, memory, and disk I/O of
each process on each node.

Some environments need extra privileges for the scraper to read host processes
fully. See [Troubleshooting](#troubleshooting) for commonly observed issues. The
[values example](../examples/host-process-metrics.values.yaml) enables every
privilege, mount, and mute option in this guide.

## Enable the scraper

The node Collector already mounts the host's `/proc` at `/hostfs/proc`. Add the
receiver and its own pipeline to the metrics gateway:

```yaml
daemonset:
  otelcol:
    config:
      receivers:
        hostmetrics:
          root_path: /hostfs
          collection_interval: 1m
          scrapers:
            process:
              resource_attributes:
                process.command_line:
                  enabled: false
      service:
        pipelines:
          metrics/process:
            receivers: [hostmetrics]
            processors: [memory_limiter, resource/node]
            exporters: [otlp_http/metrics]
```

A separate pipeline avoids copying the default `metrics` pipeline's `receivers`
list, which Helm would replace rather than merge. Keep `process.command_line`
disabled: command lines can contain credentials.

## Security

The node Collector runs as root on every node with the host's `/proc` mounted.
The privileges in [Troubleshooting](#troubleshooting) widen what a compromised
Collector can reach:

- `SYS_PTRACE` lets it read the environment variables and memory
  (`/proc/<pid>/environ`, `/proc/<pid>/mem`) of host processes running as root,
  such as the kubelet and the container runtime.
- `DAC_READ_SEARCH` extends that to every user's processes and bypasses read
  permissions on every mounted host path. It also enables
  `open_by_handle_at(2)`, which the default seccomp profile then allows, to
  open any file on a host filesystem the Collector has a mount from.
- An `Unconfined` AppArmor profile, or `spc_t` under SELinux, removes that layer
  for the whole container.

Grant only what you need. `SYS_PTRACE` alone covers executable paths and disk
I/O of processes running as root.

## Cardinality

Each process is its own resource, keyed by `process.pid`, and the default
metrics produce seven series per process. Kernel threads count as processes,
and the kernel runs several per CPU.

Every new process starts new series, so short-lived processes add churn. The
scraper's CPU use also grows with the number of processes.

Prometheus OTLP ingestion does not turn resource attributes, other than service
identity, into series labels unless they are promoted, so series from different
processes collide. Promote the `process.*` attributes in the backend, or copy
the ones that identify a process to data point attributes in the
`metrics/process` pipeline:

```yaml
daemonset:
  otelcol:
    config:
      processors:
        transform/process:
          metric_statements:
            - context: datapoint
              statements:
                - set(attributes["process.pid"], resource.attributes["process.pid"])
                - set(attributes["process.executable.name"], resource.attributes["process.executable.name"])
      service:
        pipelines:
          metrics/process:
            processors: [memory_limiter, resource/node, transform/process]
```

To reduce volume:

- Filter on `process.executable.name` with `include` or `exclude`:

  ```yaml
  daemonset:
    otelcol:
      config:
        receivers:
          hostmetrics:
            scrapers:
              process:
                include:
                  match_type: regexp
                  names: ["^kubelet$", "^containerd", "^crio$"]
  ```

- Disable metrics you do not need under `metrics`.
- Set `scrape_process_delay` to skip short-lived processes.
- Raise `collection_interval` to send fewer samples. The series count does not
  change.

## Troubleshooting

The scraper emits what it can read and, on every collection, logs an
`Error scraping metrics` line for the rest, counted in
`otelcol_scraper_errored_metric_points`. CPU time and memory need no extra
privileges. To see the errors:

```console
kubectl logs --namespace <release-namespace> daemonset/nktp-daemonset-collector \
  --container otc-container | grep "Error scraping metrics"
```

Read [Security](#security) before granting a privilege, or see
[Mute errors](#mute-errors) to accept the missing data.

### Permission denied reading executable paths

```text
error reading process executable for pid <pid>: readlink /hostfs/proc/<pid>/exe: permission denied
```

`process.executable.path` is empty for these processes. Reading
`/proc/<pid>/exe` requires passing a ptrace read check. The Collector runs as
root with its capabilities dropped, and fails that check for other users'
processes and for processes holding capabilities it lacks, such as many host
daemons. Add `SYS_PTRACE`, keeping `SYS_CHROOT` for the journald receiver:

```yaml
daemonset:
  kubernetes:
    securityContext:
      capabilities:
        add: [SYS_CHROOT, SYS_PTRACE]
```

`no such file or directory` instead of `permission denied` comes from kernel
threads, which have no executable.

### Permission denied with SYS_PTRACE

On nodes with AppArmor, the runtime's default profile can deny these reads even
with `SYS_PTRACE`. The containerd and Docker profiles, for example, allow ptrace
reads only within the same profile. The node's kernel or audit log then shows
`apparmor="DENIED" operation="ptrace"`. If it does, run the Collector
unconfined:

```yaml
daemonset:
  kubernetes:
    securityContext:
      appArmorProfile:
        type: Unconfined
```

### Permission denied reading disk I/O

```text
error reading disk usage for process "<name>" (pid <pid>): open /hostfs/proc/<pid>/io: permission denied
```

`process.disk.io` is missing for these processes. Reading `/proc/<pid>/io`
needs `SYS_PTRACE`. For processes that do not run as root, it also needs
`DAC_READ_SEARCH`, the broadest of these privileges:

```yaml
daemonset:
  kubernetes:
    securityContext:
      capabilities:
        add: [SYS_CHROOT, SYS_PTRACE, DAC_READ_SEARCH]
```

### Process owner lookups fail

```text
error reading username for process "<name>" (pid <pid>): open /etc/passwd: no such file or directory
```

`process.owner` is empty. The scraper resolves user IDs through the Collector
container's own `/etc/passwd`, which the default image lacks. Mount the node's
`/etc/passwd` read-only at `/etc/passwd`. Volumes and mounts are lists, so copy
the defaults from `nktp/deploy/charts/nktp/values.yaml` and add:

```yaml
daemonset:
  kubernetes:
    volumes:
      # ...default volumes...
      - name: host-passwd
        hostPath:
          path: /etc/passwd
          type: File
    volumeMounts:
      # ...default volume mounts...
      - name: host-passwd
        mountPath: /etc/passwd
        readOnly: true
```

Users added to the node later resolve after the Collector restarts. Processes in
containers have user IDs defined in their own images. Those IDs either fail as
below or resolve to an unrelated node user:

```text
error reading username for process "<name>" (pid <pid>): user: unknown userid <uid>
```

## Mute errors

Each option stops one kind of error from being logged. The data stays missing.

| Option | Errors from | Use for |
| --- | --- | --- |
| `mute_process_exe_error` | `/proc/<pid>/exe` | Kernel threads. Also hides missing `SYS_PTRACE` and AppArmor denials. |
| `mute_process_io_error` | `/proc/<pid>/io` | Running without `SYS_PTRACE`, or without `DAC_READ_SEARCH` for processes that do not run as root. |
| `mute_process_user_error` | Owner lookup | Running without the `/etc/passwd` mount, or container user IDs. |

```yaml
daemonset:
  otelcol:
    config:
      receivers:
        hostmetrics:
          scrapers:
            process:
              mute_process_exe_error: true
              mute_process_io_error: true
              mute_process_user_error: true
```

Or disable the metric or attribute you cannot read. The scraper looks up owners
even with `process.owner` disabled, so keep `mute_process_user_error`:

```yaml
daemonset:
  otelcol:
    config:
      receivers:
        hostmetrics:
          scrapers:
            process:
              mute_process_user_error: true
              metrics:
                process.disk.io:
                  enabled: false
              resource_attributes:
                process.owner:
                  enabled: false
```
