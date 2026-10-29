> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get clusters

List tenant clusters on the management plane

### Aliases

`cluster`

### Synopsis

List tenant clusters on the management plane.

Naming clusters narrows the listing to them, in the order given.

Example — every cluster:
```
nvkectl get clusters
```

Example — two clusters, as YAML:
```
nvkectl get clusters prod staging -o yaml
```

```
nvkectl get clusters [name ...] [flags]
```

### Options

```
  -h, --help            help for clusters
  -o, --output string   Output format: json or yaml (default: table)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl get](nvkectl_get.md)	 - Display one or more resources

