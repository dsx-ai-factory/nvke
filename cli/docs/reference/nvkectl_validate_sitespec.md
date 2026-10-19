> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl validate sitespec

Check a site spec the way nvkectl bootstrap site reads it

### Required options

`--sitespec`

### Synopsis

Check a site spec: every field is validated. With spire.upstreamCA: true,
NVKECTL_SPIRE_CA_CERT_FILE and NVKECTL_SPIRE_CA_KEY_FILE must both name
readable files; without it, they must be unset. The command prints "valid"
and exits 0 when the file is valid, and reports every problem otherwise. It
says so when spire.trustDomain is not set, since the bootstrap and the
controller stack install then both use the default.

With infra.provider nico, the bootstrap also reads what only its host holds:
the file NVKECTL_NICO_CREDENTIALS names, the files infra.nico names, the node
login in NVKECTL_NODE_USER and NVKECTL_NODE_PASSWORD, spire.externalAddress
(required unless --disable-mtls is given, as it is for "nvkectl bootstrap
site") and the https check on infra.nico.remote.endpoint. Those are reported
but do not fail the run, because the controller stack install runs this
command on a host that does not have them. --bootstrap-only fails on them, so
a file it passes is one "nvkectl bootstrap site" accepts on this host.

```
nvkectl validate sitespec [flags]
```

### Examples

```
  nvkectl validate sitespec -f sitespec.yaml
  nvkectl validate sitespec -f sitespec.yaml --bootstrap-only
  nvkectl validate sitespec -f sitespec.yaml --bootstrap-only --disable-mtls
```

### Options

```
      --bootstrap-only    Also fail on what only the bootstrap host holds, as "bootstrap site" does
      --disable-mtls      Check the file as "bootstrap site --disable-mtls" would: with infra.provider nico, spire.externalAddress is not required
  -h, --help              help for sitespec
  -f, --sitespec string   Path to the site spec (required)
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl validate](nvkectl_validate.md)	 - Validate a file before acting on it

