> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl config use-site

Switch the active site to a previously configured entry

### Synopsis

Set current-site in ~/.config/nvkectl/config.yaml to a previously registered site, without re-registering it.

```
nvkectl config use-site <name> [flags]
```

### Options

```
  -h, --help   help for use-site
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl config](nvkectl_config.md)	 - Manage nvkectl configuration

