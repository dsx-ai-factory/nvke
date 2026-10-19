> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get kubeconfig

Get the admin kubeconfig for a tenant cluster

### Synopsis

Get the admin kubeconfig for a tenant cluster from the management cluster.

The kubeconfig is read from the Secret that the NVKE tenantaccess controller
publishes into the management cluster at:

```
<namespace>/<cluster-name>-admin-kubeconfig  (key: value)
```

The namespace defaults to nvke-system, falling back to nke-system, which is
where a site installed before the namespace became configurable publishes.
Use --namespace to name it outright.

The management cluster kubeconfig is the active site's configured kubeconfig
(see 'nvkectl config set-site --kubeconfig'); use --site to target a
different, previously configured site for this invocation.

By default the kubeconfig is printed to stdout. Use --output-file to write it
to a file instead.

Example:
```
nvkectl get kubeconfig my-cluster
nvkectl get kubeconfig my-cluster --output-file ~/.kube/my-cluster.yaml
nvkectl get kubeconfig my-cluster --site prod
nvkectl get kubeconfig my-cluster --namespace nke-system
```

```
nvkectl get kubeconfig <cluster-name> [flags]
```

### Options

```
  -h, --help                 help for kubeconfig
      --namespace string     Management namespace holding the kubeconfig Secret (default: nvke-system, then nke-system)
      --output-file string   Write kubeconfig to file instead of stdout
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl get](nvkectl_get.md)	 - Display one or more resources

