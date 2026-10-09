# Test coverage

What this repository collects, what it does with it, and — the part that matters
most — what the number does not mean.

## The policy in four sentences

Every module that has tests writes a coverage profile. CI validates those
profiles, publishes a per-module table, and attaches the profiles to the run.
**No threshold gates a merge, and none is planned.** What does fail a merge is a
profile that is missing, malformed or empty, and a test package that nothing is
running.

## A percentage is not test adequacy

Coverage measures which lines a test binary executed. That is all it measures.
It does not know whether a single assertion was made about any of them.

```go
func TestParse(t *testing.T) {
    _, _ = Parse(input) // 100% of Parse is now "covered"
}
```

That test executes every line of `Parse` and would pass if `Parse` returned
nonsense for every input. A suite of tests like it reports the same number as a
suite that checks every result, every error path and every boundary. The two are
not equally good, and no percentage distinguishes them.

The reverse is just as true. A table-driven test that exercises four
representative inputs out of a space of thousands reports exactly the coverage of
the lines it ran, and says nothing about the inputs it did not try. Coverage has
no concept of the case you did not think of — which is where the bugs are.

So:

- **Do not use the number to argue a change is tested.** Say what the tests
  assert.
- **Do not use the number to argue a change is untested.** Point at the behaviour
  nothing checks.
- **Do not chase the number.** Tests written to raise a percentage are tests
  written to execute lines, which is the one property that has already been shown
  not to matter.

## What it is good for

Two things, both about change rather than about level.

**Movement.** A module that drops ten points in one pull request is worth a
question. The answer is sometimes "a large well-tested package was deleted" and
sometimes "the tests stopped running". Both are worth knowing; neither is visible
without collection.

**Silently unrun tests.** This is the failure the collection exists for. A build
tag, a renamed directory, a package dropped from a `go test` argument list — any
of them takes a test suite out of the build, and takes its failures with it. The
run still passes. Nothing is red. `hack/coverage.sh` fails when a package that
holds `_test.go` files reaches no profile, so the suite that stopped running is
an error rather than an absence.

This has already happened here. Before controller-test.yml existed, `agent` was
the only module anything ran `go test` on, while five others were compiled,
linted, vetted and scanned — and never tested. Two `cloudinit` tests under
`controller/test/kubeadm` pointed at a path that does not exist and failed on
every invocation, while CI stayed green, because the build tag that gates them
was set nowhere.

## How it is wired

| Piece | What it owns |
|---|---|
| [`make/coverage.mk`](../make/coverage.mk) | The profile path, the directory and `-covermode`. Included by every collecting module's Makefile. |
| each module's `test` target | Appends `$(GO_COVER_FLAGS)` to its own `go test`. The package arguments stay the module's business. |
| [`hack/coverage.sh`](../hack/coverage.sh) | The registry of which modules collect, the checks, and the report. |
| [`.github/workflows/coverage.yml`](../.github/workflows/coverage.yml) | Runs it in CI, writes the table to the job summary, uploads the profiles. |

Profiles land in `coverage/` at the repository root, one per module
(`coverage/controller.out`, `coverage/agent.out`, …), gitignored. The name is the
module path with the separators flattened. `management-engine` writes two,
because its API suite and its envtest suite are separate invocations that cannot
share an output file.

### Running it

```bash
make coverage          # run every collecting module's tests, then report
make coverage-report   # report on the profiles already in coverage/
make coverage-check    # check the registry against the tree; runs no tests
```

`make coverage-check` is the cheap one, and the one in the merge gate. It needs
no test run and takes about a second.

To read a profile:

```bash
go tool cover -html=coverage/controller.out        # in a browser
go tool cover -func=coverage/controller.out        # per function, on stdout
```

### Which modules collect

Nine of the eleven workspace modules. `controller/api` holds generated OpenAPI
types and a generated client, and `controller/hack/capd-reboot-controller` is a
development helper run by hand; neither has a test file. Both are declared as
such in `hack/coverage.sh`, and `make coverage-check` fails if either gains one
without being moved — a module that starts testing and does not start collecting
is the drift this is here to catch.

## What fails, and what it means

| Failure | What happened |
|---|---|
| `no coverage profile in coverage` | The module's tests did not run, or ran without `$(GO_COVER_FLAGS)`. |
| `not a \`mode:\` header` / `malformed block line(s)` | The profile is truncated or is not a coverage profile. `go tool cover` would reject it, and a check that only tested for the file's existence would not. |
| `the profile holds 0 statements` | The suite compiled and measured nothing. |
| `holds tests that the default build does not compile` | A build tag excludes a suite. Declare it in `untested_packages()` with the reason, or drop the tag. |
| `holds tests and compiles, but reaches no coverage profile` | The module's test target never reached that package. |
| `waives <dir>, which no longer exists` | A stale waiver, which keeps a suite excluded long after the reason is gone. |

A package of nothing but `_test.go` files — `controller/test/e2e` itself,
`os-image/files/test`, `controller/hack/capd-kindest-node` — is exempt from the
profile checks. It has no statements of its own for coverage to attribute, so it
cannot appear in a profile however thoroughly it runs. It is still compiled and
still fails the suite when it breaks.

## Deliberate non-choices

**No threshold, now or later.** A threshold turns the number into a thing to
satisfy, and the cheapest way to satisfy it is the assertion-free test above. If
one is ever adopted it needs a named owner and a written statement of what it is
evidence of, which is a harder document to write than it looks — this one is the
attempt, and it concludes that the threshold is not worth having.

**No external coverage service.** Codecov or Coveralls would give trend graphs
and a pull-request comment for a third-party app with repository access and a
token. The job summary and the uploaded artifact answer the same questions for
this repository's size, and the previous cobertura conversion was removed when
the pipeline that consumed it went away rather than being replaced. If the trend
view is ever wanted, that is the moment to reconsider — not before.

**No `-coverpkg=./...`.** Attributing one package's tests to another package's
statements makes an integration test look like unit coverage of code it merely
passes through, which is the most misleading reading of a number that is already
easy to misread.

## See also

- [development.md](development.md) — the checks to run before opening a pull
  request
- [go-workspace.md](go-workspace.md) — the modules this is collected across
- [../.github/workflows/README.md](../.github/workflows/README.md) — how CI is
  wired
