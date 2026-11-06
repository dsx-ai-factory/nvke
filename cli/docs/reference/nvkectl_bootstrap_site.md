> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl bootstrap site

Bootstrap a new NVKE management cluster from a site spec file

### Required options

`--sitespec`

### Synopsis

Bootstrap a vanilla Kubernetes cluster from the nodes described by the site
spec file passed with -f/--sitespec: pre-provisioned hosts it lists
(infra.provider provisioned, the default), or machines CAPNICo allocates from a
NICo site (infra.provider nico).

The site spec file is loaded and validated first. On any invalid or missing
field the command fails immediately with a clear error, before any workflow is
dispatched or any node is touched.

The bootstrap side-loads the images this repository builds, nke-controller:latest
and, with the provisioned provider, cluster-api-provider-provisioned:latest,
which "make -C cli images" builds. With nico, CAPNICo's image is public and
pulled by the cluster; --release pins it to an NVKR release's artifact. --nvke-controller-image and --provisioned-provider-image, or
NVKECTL_NVKE_CONTROLLER_IMAGE and NVKECTL_CAPI_PROVISIONED_IMAGE, name other images, such as
ones pushed to a registry; a flag wins over its variable. cli/README.md lists
every NVKECTL_* variable.

Every node becomes a control-plane node. Once the cluster is up, the
control-plane NoSchedule taint is removed from each node so workloads can run.

With nico, infra.nico names the NICo and the MachineImage and ClusterRelease
manifests the controller needs; the NICo token is read from the file the
NVKECTL_NICO_CREDENTIALS variable names; the login rendered into every node
comes from NVKECTL_NODE_USER and NVKECTL_NODE_PASSWORD; and
spire.externalAddress is required unless --disable-mtls is set. The nodes'
names and addresses are not printed; read them with "kubectl get nodes -o wide"
on the finished cluster.

See the bundled examples/sitespec.yaml and docs/sitespec.md for the
site spec file format.

```
nvkectl bootstrap site [flags]
```

### Examples

```
  # Validate and bootstrap from a site spec file
  nvkectl bootstrap site -f sitespec.yaml

  # Validate only — load, check, and print the site spec without dispatching
  nvkectl bootstrap site -f sitespec.yaml --dry-run

  # Deploy images you pushed to a registry instead of the local build
  nvkectl bootstrap site -f sitespec.yaml \
    --nvke-controller-image nvcr.io/<org>/nvke-controller:edge \
    --provisioned-provider-image nvcr.io/<org>/cluster-api-provider-provisioned:edge

  # Export this run's spans and log records to a collector the cluster can reach
  nvkectl bootstrap site -f sitespec.yaml --otlp-endpoint collector.example.com:4317
```

### Options

```
      --allow-control-plane-without-vip     Permit a multi-node control plane without a VIP and enable the override in the temporary NVKE controller. Development only; replacing the initializing node can break API access.
      --arch string                         Architecture of the release artifact to use, linux/amd64 or linux/arm64; default: the bootstrap host's, since the bootstrap CAPNICo runs in its Kind cluster. Only with --release.
      --diagnostics-dir string              Directory a failed bootstrap writes cluster diagnostics to before tearing the cluster down. With --verbose a successful bootstrap writes them too. Overrides NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR. (default "cluster-diagnostics")
      --disable-mtls                        Stand the site up without SPIFFE: no SPIRE, and the controller dials node agents in plaintext. The agents must be running with --disable-mtls too. Development only.
      --dry-run                             Load and validate the site spec and print it, but do not dispatch any workflow
      --encryption-key-out string           Where to write the finished cluster's etcd encryption key, as a Secret manifest (default: <clusterName>-etcd-encryption-key.json in the working directory)
  -h, --help                                help for site
      --keep-bootstrap-cluster              Leave the ephemeral bootstrap Kind cluster running after the command exits, for debugging
      --kubeconfig-out string               Where to write the finished cluster's admin breakglass kubeconfig (default: <clusterName>.kubeconfig in the working directory)
      --nvke-controller-image string        NVKE controller image to deploy (overrides NVKECTL_NVKE_CONTROLLER_IMAGE and the manifest default)
      --otlp-endpoint string                OTLP/gRPC endpoint, a host:port or an http(s):// URL (http implies --otlp-insecure), that receives this run's spans and log records. The temporary controller in the bootstrap cluster exports there too, so a loopback address is rejected
      --otlp-insecure                       Send to --otlp-endpoint without TLS; requires --otlp-endpoint
      --provisioned-provider-image string   Provisioned-provider image to deploy (overrides NVKECTL_CAPI_PROVISIONED_IMAGE and the manifest default)
      --release string                      NVKR release description whose capnico-controller artifact replaces the bootstrap CAPNICo image with its pinned digest, as "render deploy-path --release" does for the management cluster's CAPNICo. The release description is not verified: run "nvkr release verify" on the same reference first. Only with infra.provider nico.
  -f, --sitespec string                     Path to the site spec YAML file (required)
      --timeout duration                    How long to wait for the cluster to finish provisioning, which includes every control plane node joining. Overrides NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT. (default 30m0s)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl bootstrap](nvkectl_bootstrap.md)	 - Bootstrap NVKE clusters from a declarative site spec

