> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl get bootstrap-manifest

Print the charts and images the bootstrap fetches, with version and digest

### Synopsis

Print the bootstrap manifest this nvkectl was built with to standard output.

The manifest lists the charts "nvkectl bootstrap site" fetches and installs
with Helm, and the images the bootstrap runs. nvkectl installs each chart at
the listed version. The digests are a record for mirroring. The bootstrap
does not verify them.

Not listed: the controller chart, the Postgres chart and the provisioned
provider's manifest, which ship inside nvkectl. Also not listed: the
components "clusterctl init" installs, and the kindest/node image.

```
nvkectl get bootstrap-manifest [flags]
```

### Examples

```
  nvkectl get bootstrap-manifest
```

### Options

```
  -h, --help   help for bootstrap-manifest
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl get](nvkectl_get.md)	 - Display one or more resources

