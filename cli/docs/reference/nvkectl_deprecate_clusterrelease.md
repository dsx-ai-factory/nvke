> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl deprecate clusterrelease

Deprecate a cluster release

### Aliases

`clusterreleases`

### Synopsis

Deprecate a cluster release on the active site's management cluster.

A deprecated release keeps resolving, so clusters and node pools already built
from it keep provisioning and scaling. It is never chosen as a Kubernetes
upgrade target, and new clusters and node pools should not select it. Only
spec.lifecycle.deprecated changes; the release content is immutable.

Example:
```
nvkectl deprecate clusterrelease <name>
```

```
nvkectl deprecate clusterrelease <name> [flags]
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

* [nvkectl deprecate](nvkectl_deprecate.md)	 - Deprecate a release

