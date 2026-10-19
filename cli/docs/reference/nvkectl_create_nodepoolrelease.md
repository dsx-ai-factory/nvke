> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl create nodepoolrelease

Create a node pool release: a worker release

### Aliases

`nodepoolreleases`

### Required options

`--filename`

### Synopsis

Create a node pool release on the active site's management cluster.

The request is a single YAML or JSON document with a metadata and a spec, and
nothing else. nvkectl supplies the apiVersion and kind, so leave them out:

```
metadata:
  name: <name>
spec:
  ...
```

Pass a file path or '-' to read from stdin. Unknown fields are rejected.

Published content is immutable. To change what a node pool release provisions, create it
again under a new name; creating an existing name fails. A release must name a
machine image that already exists, so create the machine image first.

With --dry-run=client the document is checked and printed as a complete
manifest, and no management cluster is contacted, so no site is needed. The
check covers structure only: required fields and value rules are enforced by the
management cluster when the resource is created, and the machine image a
release names is not looked up.

Example:
```
nvkectl create nodepoolrelease -f nodepoolrelease.yaml
```

Example - print the complete manifest without creating anything:
```
nvkectl create nodepoolrelease -f nodepoolrelease.yaml --dry-run=client -o yaml
```

```
nvkectl create nodepoolrelease [flags]
```

### Options

```
      --dry-run string    Use "client" to print the complete manifest without contacting the management cluster (default "none")
  -f, --filename string   Path to the YAML or JSON request, or '-' to read from stdin
  -h, --help              help for nodepoolrelease
  -o, --output string     Output format: json or yaml (default: a one-line message, or yaml with --dry-run)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl create](nvkectl_create.md)	 - Create a resource

