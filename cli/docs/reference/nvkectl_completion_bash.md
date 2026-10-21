> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

## nvkectl completion bash

Generate the autocompletion script for bash

### Synopsis

Generate the autocompletion script for the bash shell.

This script depends on the 'bash-completion' package.
If it is not installed already, you can install it via your OS's package manager.

To load completions in your current shell session:

```
source <(nvkectl completion bash)
```

To load completions for every new session, execute once:

#### Linux:

```
nvkectl completion bash > /etc/bash_completion.d/nvkectl
```

#### macOS:

```
nvkectl completion bash > $(brew --prefix)/etc/bash_completion.d/nvkectl
```

You will need to start a new shell for this setup to take effect.


```
nvkectl completion bash
```

### Options

```
  -h, --help              help for bash
      --no-descriptions   disable completion descriptions
```

### Options inherited from parent commands

```
      --json-logs     Emit logs as JSON
      --site string   Override the active site for this invocation
  -v, --verbose       Enable debug logging
```

### SEE ALSO

* [nvkectl completion](nvkectl_completion.md)	 - Generate the autocompletion script for the specified shell

