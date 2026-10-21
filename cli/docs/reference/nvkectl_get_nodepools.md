> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get nodepools

List node pools on the management plane

### Aliases

`nodepool`

### Synopsis

List node pools on the management plane.

Naming pools narrows the listing to them, in the order given. Node pool names
are unique within a cluster, so a name on its own can match a pool in every
cluster; pass --cluster as well to select one.

Example — every pool of one cluster:
```
nvkectl get nodepools --cluster my-cluster
```

Example — two pools by name:
```
nvkectl get nodepools control-plane gpu-workers --cluster my-cluster
```

Example — one pool, as YAML:
```
nvkectl get nodepools gpu-workers --cluster my-cluster -o yaml
```

```
nvkectl get nodepools [name ...] [flags]
```

### Options

```
  -c, --cluster string   Filter output to node pools of the named cluster
  -h, --help             help for nodepools
  -o, --output string    Output format: json or yaml (default: table)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl get](nvkectl_get.md)	 - Display one or more resources

