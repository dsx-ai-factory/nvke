> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl patch cluster

Patch a tenant cluster

### Aliases

`clusters`

### Synopsis

Patch a tenant cluster on the management plane.

PATCH semantics apply: only the fields in the patch document are changed;
everything else on the cluster is left as it is. Pass the patch inline with
--patch, or from a file (or stdin via -) with --patch-file. Exactly one of
the two is required.

Example — bump the Kubernetes version:
```
nvkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'
```

Example — patch file:
```
nvkectl patch cluster my-cluster --patch-file patch.yaml
```

Example — set the control-plane node count:
```
nvkectl patch cluster my-cluster --patch 'controlPlaneConfig: {desiredCount: 3}'
```

The patch document's fields must match the controller UpdateClusterRequest schema
exactly. See /api/docs on your management plane.

```
nvkectl patch cluster <name> [flags]
```

### Options

```
  -h, --help                help for cluster
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

