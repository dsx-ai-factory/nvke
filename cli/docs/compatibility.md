# nvkectl compatibility policy

Anything you script against `nvkectl` is a contract whether or not anyone
declared it one. This document declares it: what nvkectl promises not to break,
what it reserves the right to change, and how something that has to change gets
removed.

`nvkectl` versions and releases on its own `cli/vX.Y.Z` tags, separately from the
controller. The [release process](../../RELEASE.md) defines what each increment
means. This document says which parts of the CLI those increments apply to.

`cmd/nvkectl/contract_test.go` enforces the promises below at the command
boundary — it builds the binary and runs it as a process, so it sees the exit
code, the stream each byte lands on, and what a signal does. A change that
breaks one of these promises fails that test. Changing the test to match the new
behaviour, without the deprecation below, is the thing this policy exists to
prevent.

## What is contract

These are covered by semantic versioning. A change that breaks one is a MAJOR
change, and reaches a release only through the deprecation process at the end of
this document.

| Surface | What is promised |
|---|---|
| **Command and subcommand names** | `version`, `config`, `bootstrap`, `generate`, `get`, `create`, `patch`, `delete` and their subcommands keep their names and their argument shape. Aliases already published (`cluster`/`clusters`, `nodepool`/`nodepools`) keep working. |
| **Flag names and semantics** | A documented flag keeps its name, its short form, and its meaning. New flags may be added. A flag's default may change only in a MINOR release and only when the new default cannot break a scripted invocation. |
| **Exit codes** | The table below. |
| **Output channels** | Results go to stdout. Diagnostics, progress, logs and deprecation warnings go to stderr. The two never mix: an error message never goes to stdout, so a piped `-o json` is never polluted by one. A failing command writes nothing to stdout, with one exception: `get clusters` and `get nodepools` given several names print the ones that matched, kubectl-style, and exit `1` if any name was not found. A query command that succeeds writes nothing to stderr, except the trace ID mismatch warning; a command that does work, such as `bootstrap`, reports its progress there. The trace ID is reported on stderr only, as described under [Trace ID](#trace-id). |
| **Machine-readable output** | `-o json` and `-o yaml` keep the field names they emit. `nvkectl version -o short` stays the bare version string and nothing else. |
| **Config file** | The location (`$XDG_CONFIG_HOME/nvkectl/config.yaml`, else `~/.config/nvkectl/config.yaml`) and the `sites` / `current-site` schema. Fields may be added; existing ones are not renamed or repurposed. The binary was called `nkectl` until cli/v0.1.x and kept the same file under `nkectl/`; when the current location has no file yet, that one is read and copied over once. |
| **Environment variables** | The `NVKECTL_*` variables, as documented in [the README](../README.md), keep their names and meanings. Each `NVKECTL_*` variable is also read under its `NKECTL_*` name for one release, with a deprecation warning, except `NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT`, which turns a safety check off and answers only to its own name. |
| **Site spec schema** | `generate sitespec` output and `bootstrap site -f` input, as documented in [sitespec.md](sitespec.md). |

### Trace ID

Every command except `help` and `completion` runs under one trace ID. nvkectl sends it to the site controller in the `traceparent` header, and every log line of the run carries it as `trace_id`. How it is reported depends on the kind of command:

| Kind | On success | On failure |
|---|---|---|
| **Query commands:** commands that only report or write files (`get`, `version`, `config`, `generate`, `render`) | Nothing printed; `-v` logs the ID | The error ends with `Trace ID: <32 hex digits>` |
| **Help:** `-h`, `--help`, a bare group such as `nvkectl create`, `help`, `completion` | Nothing printed | Not applicable |
| **Everything else** (`create`, `delete`, `patch`, `bootstrap`, `validate`, `deprecate <kind>`) | The last line of stderr is `Trace ID: <32 hex digits>`; `bootstrap site` also prints it before it starts | The error ends with the same line |

The line goes to stderr only, so `-o json` and `-o yaml` output is never changed. A command cancelled by `SIGINT` or `SIGTERM` ends with it too. An unknown command or flag at the root (`nvkectl bogus`, `nvkectl --nosuchflag`) prints the bare error. If the controller answers with an `X-Trace-ID` that differs from the one nvkectl sent, nvkectl writes one warning to stderr per run, even for a query command, naming both IDs; check that `traceparent` reaches the controller and that the controller continues the incoming trace.

## What is not contract

Depend on these and a patch release may break you.

- **Human-readable table output.** Column set, column order, widths, and the
  `AGE` bucketing are presentation. Use `-o json` for anything scripted.
- **Error message text.** The exit code and the stream are contract; the wording
  is not. Match on the exit code, not on a substring of stderr.
- **Help and usage text**, including the order commands are listed in.
- **Log output.** `--verbose` and `--json-logs` exist for a human reading a
  failure. The fields, their names, and the lines emitted at each level may
  change at any time.
- **Anything under `internal/`.** Go's own rules make this unimportable; it is
  listed so nobody vendors it by copy.
- **Alpha commands**, which say so in their help text. `nvkectl get providers` is
  alpha today.
- **Timing and ordering** of the steps a long-running command prints as it runs.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | The command succeeded. |
| `1` | The command failed. Invalid input, a usage error, an unreachable or erroring site controller, and a failed operation all report `1` — nvkectl does not distinguish them by code. The reason is on stderr. |
| `130` | The command was interrupted by `SIGINT` and stopped before finishing. `128 + SIGINT`, following shell convention. |
| `143` | The command was interrupted by `SIGTERM` and stopped before finishing. `128 + SIGTERM`. |

Nothing else is returned deliberately. A code outside this table means the
process was killed by a signal nvkectl does not handle, and the shell reported
`128 + signum`.

**`130` and `143` do not mean nothing happened.** Interrupting `bootstrap site` part way
may leave the site cluster it already created; nvkectl does not roll it back.
Before site-cluster creation has started, nvkectl tears down the bootstrap Kind
cluster unless `--keep-bootstrap-cluster` is set. After it has started, nvkectl
retains the bootstrap cluster for recovery and prints how to recover it.

## Signals

The first `SIGINT` or `SIGTERM` cancels the running command's context. A command
that is waiting on the network or on a child process unwinds, cleans up, and
exits `130` for `SIGINT` or `143` for `SIGTERM`.

A second signal kills the process outright. nvkectl restores the default
disposition as soon as the first one arrives, so a command blocked somewhere
that cannot watch a context — reading a spec from a terminal, for instance —
still answers to a repeated Ctrl-C.

## Non-interactive use

`nvkectl` never prompts. Every command runs unattended, in CI, with stdin closed:

- Nothing asks for confirmation. `nvkectl delete cluster` deletes.
- Nothing reads from stdin unless you passed `-f -`.
- Nothing requires a TTY, and no behaviour changes based on whether one is
  attached.

Credentials come from the config file, the kubeconfig it names, and the
documented environment variables — never from a prompt.

## Deprecation and removal

A flag, command, output field or environment variable that has to go follows
this sequence. It is tied to the increments in [RELEASE.md](../../RELEASE.md).

1. **Announce**, in a MINOR release. The deprecated thing keeps working and
   keeps its behaviour. Its help text says it is deprecated, says what replaces
   it, and names the release it was deprecated in. Using it writes a one-line
   warning **to stderr** — never to stdout, so a piped `-o json` is unaffected.
   The `CHANGELOG.md` entry says the same.
2. **Carry it** for at least one further MINOR release. A replacement, where one
   exists, ships in the same release as the announcement, so there is never a
   window in which neither works.
3. **Remove**, in a MAJOR release only, with the removal in the release notes.

Adding a command, a flag, an output field, or an environment variable is a MINOR
change and needs none of this. Fixing a command that contradicted its own
documentation is a PATCH change: the documented behaviour is the contract, and
the bug was the deviation.

A security fix may skip the carry period. The release notes say so explicitly
when it does.

## Reporting a break

If a release broke something on the contract list, that is a bug — open an issue
on [the repository](https://github.com/dsx-ai-factory/nvke/issues) with the
command, the version you moved from and to, and what changed. If it broke
something on the "not contract" list, an issue is still useful: it says the line
is in the wrong place.
