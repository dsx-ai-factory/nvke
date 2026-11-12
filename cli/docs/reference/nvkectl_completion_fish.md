> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl completion fish

Generate the autocompletion script for fish

### Synopsis

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

```
nvkectl completion fish | source
```

To load completions for every new session, execute once:

```
nvkectl completion fish > ~/.config/fish/completions/nvkectl.fish
```

You will need to start a new shell for this setup to take effect.


```
nvkectl completion fish [flags]
```

### Options

```
  -h, --help[=true|false]              help for fish
      --no-descriptions[=true|false]   disable completion descriptions
```

### Options inherited from parent commands

```
      --json-logs[=true|false]   Emit logs as JSON
      --site string              Override the active site for this invocation
  -v, --verbose[=true|false]     Enable debug logging
```

### SEE ALSO

* [nvkectl completion](nvkectl_completion.md)	 - Generate the autocompletion script for the specified shell

