> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get machineimages

List machine images on the active site's management cluster

### Aliases

`machineimage`

### Synopsis

List machine images on the active site's management cluster.

Naming machine images narrows the listing to them, in the order given.

With -o json or -o yaml, one name prints that machine image as a single object in the
metadata and spec shape that 'nvkectl create machineimage' accepts, so it can be edited and
created under a new name. No name, or several, prints a list of those objects.

Example - every machine image:
```
nvkectl get machineimages
```

Example - one machine image, as YAML:
```
nvkectl get machineimages <name> -o yaml
```

```
nvkectl get machineimages [name ...] [flags]
```

### Options

```
  -h, --help            help for machineimages
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

