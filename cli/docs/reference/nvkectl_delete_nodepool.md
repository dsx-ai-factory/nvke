> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl delete nodepool

Delete a node pool

### Aliases

`nodepools`

### Required options

`--cluster`

### Synopsis

Delete a node pool on the management plane.

Node pool names are unique within a cluster, so --cluster is required to
identify which pool to delete.

Deleting the last joined control-plane node pool of a cluster is rejected;
delete the cluster instead.

Example:
```
nvkectl delete nodepool gpu-workers --cluster my-cluster
```

```
nvkectl delete nodepool <name> --cluster <cluster> [flags]
```

### Options

```
  -c, --cluster string   Name of the cluster that owns the node pool
  -h, --help             help for nodepool
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl delete](nvkectl_delete.md)	 - Delete a resource

