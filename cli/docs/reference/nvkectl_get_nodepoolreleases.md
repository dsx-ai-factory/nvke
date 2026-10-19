> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get nodepoolreleases

List node pool releases on the active site's management cluster

### Aliases

`nodepoolrelease`

### Synopsis

List node pool releases on the active site's management cluster.

Naming node pool releases narrows the listing to them, in the order given.

With -o json or -o yaml, one name prints that node pool release as a single object in the
metadata and spec shape that 'nvkectl create nodepoolrelease' accepts, so it can be edited and
created under a new name. No name, or several, prints a list of those objects.

Example - every node pool release:
```
nvkectl get nodepoolreleases
```

Example - one node pool release, as YAML:
```
nvkectl get nodepoolreleases <name> -o yaml
```

```
nvkectl get nodepoolreleases [name ...] [flags]
```

### Options

```
  -h, --help            help for nodepoolreleases
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

