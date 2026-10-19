> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl create cluster

Create a tenant cluster

### Aliases

`clusters`

### Required options

`--filename`

### Synopsis

Create a tenant cluster on the management plane.

The spec must be a YAML document whose fields match the controller
CreateClusterRequest schema exactly. See /api/docs on your management plane.

Pass a file path or '-' to read from stdin (e.g. via a pipe or heredoc).

Example:
```
nvkectl create cluster -f examples/capd-cluster.yaml
```

Example — stdin via pipe:
```
cat examples/capd-cluster.yaml | nvkectl create cluster -f -
```

```
nvkectl create cluster [flags]
```

### Options

```
  -f, --filename string   Path to YAML spec file, or '-' to read from stdin
  -h, --help              help for cluster
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl create](nvkectl_create.md)	 - Create a resource

