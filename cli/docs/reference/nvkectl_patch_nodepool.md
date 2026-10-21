> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl patch nodepool

Patch a node pool

### Aliases

`nodepools`

### Required options

`--cluster`

### Synopsis

Patch a node pool on the management plane.

Node pool names are unique within a cluster, so --cluster is required to
identify which pool to patch.

PATCH semantics apply: only the fields in the patch document are changed;
everything else on the node pool is left as it is. Pass the patch inline with
--patch, or from a file (or stdin via -) with --patch-file. Exactly one of
the two is required.

nvkectl can set desiredNodeCount so far. The API
accepts more, but anything else is rejected before the request is sent.

Example — scale a pool:
```
nvkectl patch nodepool gpu-workers --cluster my-cluster --patch 'desiredNodeCount: 8'
```

Example — patch file:
```
nvkectl patch nodepool gpu-workers --cluster my-cluster --patch-file patch.yaml
```

The patch document's fields must match the controller UpdateNodePoolRequest schema
exactly. See /api/docs on your management plane.

```
nvkectl patch nodepool <name> --cluster <cluster> [flags]
```

### Options

```
  -c, --cluster string      Name of the cluster that owns the node pool
  -h, --help                help for nodepool
  -p, --patch string        Inline patch document. Accepts YAML or JSON
      --patch-file string   Path to a YAML or JSON patch file, or '-' to read from stdin
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl patch](nvkectl_patch.md)	 - Patch a resource

