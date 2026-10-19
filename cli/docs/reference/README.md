> **Generated file -- do not edit.** This page is written from the nvkectl
> command tree in `cli/internal/nvkectl`. To change what it says, change the
> command's own metadata -- its Short, Long, Example, Args, Aliases, deprecation
> notice or flag usage strings -- and run `make -C cli docs`. An edit made here
> is lost on the next run, and `make -C cli docs-check` fails until the
> committed pages match the tree again.

# nvkectl command reference

Every public `nvkectl` command and subcommand, generated from the command tree
the binary runs. Each page states the command's purpose, how it is invoked and
what positional arguments it takes, its aliases, its examples, and its own and
inherited options with their types and defaults.

This is the complete reference. It is not a guide: it says what each command
accepts, not when to reach for it.

## Concepts and guides

The reference covers command metadata. Everything that is not command
metadata is written down here instead.

| Document | Contents |
|---|---|
| [nvkectl README](../../README.md) | Installing nvkectl, authenticating to the management plane, and every `NVKECTL_*` environment variable |
| [Compatibility and deprecation policy](../compatibility.md) | What is contract and what is not, the exit codes, and how a command or flag is removed |
| [Site spec reference](../sitespec.md) | Every field of the site spec file `bootstrap site` reads, with its type |
| [Site and tenant cluster flow](../user-flow.md) | The commands in the order an operator runs them |
| [Bootstrap troubleshooting](../bootstrap-troubleshooting.md) | Where a failed bootstrap leaves its evidence, and the common failures |
| [Management cluster bootstrap](../../../docs/site-bootstrap/overview.md) | What a bootstrap does, end to end |
| [Authenticating to the NVKE API](../../../controller/docs/authenticating-to-the-api.md) | How a client proves who it is |
| [Configuring NVKE API authentication](../../../controller/docs/configuring-api-authentication.md) | Turning authentication on, and the RBAC nvkectl needs |

## What is not here

Hidden commands, and help topics that are prose rather than commands. A hidden
command is absent from `--help` and is not part of the public CLI, so it is
absent here for the same reason.

Everything else the binary exposes is covered, including the commands cobra
provides rather than nvkectl: `help` and `completion`. A deprecated command
keeps its page, carrying its deprecation notice, for as long as it still runs.

## Commands

- [`nvkectl`](nvkectl.md) — NVKE control-plane CLI
  - [`nvkectl bootstrap`](nvkectl_bootstrap.md) — Bootstrap NVKE clusters from a declarative site spec
    - [`nvkectl bootstrap site`](nvkectl_bootstrap_site.md) — Bootstrap a new NVKE management cluster from a site spec file
  - [`nvkectl completion`](nvkectl_completion.md) — Generate the autocompletion script for the specified shell
    - [`nvkectl completion bash`](nvkectl_completion_bash.md) — Generate the autocompletion script for bash
    - [`nvkectl completion fish`](nvkectl_completion_fish.md) — Generate the autocompletion script for fish
    - [`nvkectl completion powershell`](nvkectl_completion_powershell.md) — Generate the autocompletion script for powershell
    - [`nvkectl completion zsh`](nvkectl_completion_zsh.md) — Generate the autocompletion script for zsh
  - [`nvkectl config`](nvkectl_config.md) — Manage nvkectl configuration
    - [`nvkectl config set-site`](nvkectl_config_set-site.md) — Configure the management plane the CLI connects to
    - [`nvkectl config use-site`](nvkectl_config_use-site.md) — Switch the active site to a previously configured entry
    - [`nvkectl config view`](nvkectl_config_view.md) — Print the current nvkectl configuration
  - [`nvkectl create`](nvkectl_create.md) — Create a resource
    - [`nvkectl create cluster`](nvkectl_create_cluster.md) — Create a tenant cluster
    - [`nvkectl create clusterrelease`](nvkectl_create_clusterrelease.md) — Create a cluster release: a control-plane release
    - [`nvkectl create machineimage`](nvkectl_create_machineimage.md) — Create a machine image: an immutable node image and its NICo boot script
    - [`nvkectl create nodepool`](nvkectl_create_nodepool.md) — Create a node pool
    - [`nvkectl create nodepoolrelease`](nvkectl_create_nodepoolrelease.md) — Create a node pool release: a worker release
  - [`nvkectl delete`](nvkectl_delete.md) — Delete a resource
    - [`nvkectl delete cluster`](nvkectl_delete_cluster.md) — Delete a tenant cluster
    - [`nvkectl delete clusterrelease`](nvkectl_delete_clusterrelease.md) — Delete a cluster release: a control-plane release
    - [`nvkectl delete machineimage`](nvkectl_delete_machineimage.md) — Delete a machine image: an immutable node image and its NICo boot script
    - [`nvkectl delete nodepool`](nvkectl_delete_nodepool.md) — Delete a node pool
    - [`nvkectl delete nodepoolrelease`](nvkectl_delete_nodepoolrelease.md) — Delete a node pool release: a worker release
  - [`nvkectl deprecate`](nvkectl_deprecate.md) — Deprecate a release
    - [`nvkectl deprecate clusterrelease`](nvkectl_deprecate_clusterrelease.md) — Deprecate a cluster release
    - [`nvkectl deprecate nodepoolrelease`](nvkectl_deprecate_nodepoolrelease.md) — Deprecate a node pool release
  - [`nvkectl generate`](nvkectl_generate.md) — Generate NVKE configuration files
    - [`nvkectl generate oidc-kubeconfig`](nvkectl_generate_oidc-kubeconfig.md) — Generate an oidc-login kubeconfig from an input kubeconfig
    - [`nvkectl generate sitespec`](nvkectl_generate_sitespec.md) — Generate a site spec file for a tier-0 cluster
  - [`nvkectl get`](nvkectl_get.md) — Display one or more resources
    - [`nvkectl get bootstrap-manifest`](nvkectl_get_bootstrap-manifest.md) — Print the charts and images the bootstrap fetches, with version and digest
    - [`nvkectl get clusterreleases`](nvkectl_get_clusterreleases.md) — List cluster releases on the active site's management cluster
    - [`nvkectl get clusters`](nvkectl_get_clusters.md) — List tenant clusters on the management plane
    - [`nvkectl get kubeconfig`](nvkectl_get_kubeconfig.md) — Get the admin kubeconfig for a tenant cluster
    - [`nvkectl get machineimages`](nvkectl_get_machineimages.md) — List machine images on the active site's management cluster
    - [`nvkectl get nodepoolreleases`](nvkectl_get_nodepoolreleases.md) — List node pool releases on the active site's management cluster
    - [`nvkectl get nodepools`](nvkectl_get_nodepools.md) — List node pools on the management plane
    - [`nvkectl get providers`](nvkectl_get_providers.md) — List infrastructure providers installed on the management cluster
  - [`nvkectl help`](nvkectl_help.md) — Help about any command
  - [`nvkectl patch`](nvkectl_patch.md) — Patch a resource
    - [`nvkectl patch cluster`](nvkectl_patch_cluster.md) — Patch a tenant cluster
    - [`nvkectl patch nodepool`](nvkectl_patch_nodepool.md) — Patch a node pool
  - [`nvkectl validate`](nvkectl_validate.md) — Validate a file before acting on it
    - [`nvkectl validate sitespec`](nvkectl_validate_sitespec.md) — Check a site spec the way nvkectl bootstrap site reads it
  - [`nvkectl version`](nvkectl_version.md) — Print version information
