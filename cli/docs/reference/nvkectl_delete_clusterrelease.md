> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl delete clusterrelease

Delete a cluster release: a control-plane release

### Aliases

`clusterreleases`

### Synopsis

Delete a cluster release from the active site's management cluster.

Deletion is immediate and there is no prompt.

nvkectl does not check whether a cluster still selects the cluster release,
and anything that does fails to provision or upgrade once it is gone.

To retire a release that is still in use, deprecate it instead ('nvkectl deprecate').

Example:
```
nvkectl delete clusterrelease <name>
```

```
nvkectl delete clusterrelease <name> [flags]
```

### Options

```
  -h, --help   help for clusterrelease
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl delete](nvkectl_delete.md)	 - Delete a resource

