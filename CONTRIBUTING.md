# Contributing to nke

If you are interested in contributing to nke, your contributions will fall
into three categories:
1. You want to report a bug, feature request, or documentation issue
    - File an [issue](https://github.com/NVIDIA/nke/issues/new/choose)
    describing what you encountered or what you want to see changed.
    - The maintainers will evaluate issues and triage them, scheduling
    them for a release. If you believe the issue needs priority attention,
    comment on the issue to notify the team.
2. You want to propose a new Feature and implement it
    - Post about your intended feature, and we shall discuss the design and
    implementation.
    - Once we agree that the plan looks good, go ahead and implement it, using
    the [code contributions](#code-contributions) guide below.
3. You want to implement a feature or bug-fix for an outstanding issue
    - Follow the [code contributions](#code-contributions) guide below.
    - If you need more context on a particular issue, please ask and we shall
    provide.

## Setting up your development environment

### Prerequisites

- Go 1.26+ (`go version`) — the floor is set by `go.work`, not by any single
  module. A newer Go is fine: `make` pins `GOTOOLCHAIN` to the version `go.work`
  declares, because the vendored `strcase` panics at startup on anything newer.
  Set `GOTOOLCHAIN` yourself to override that.
- GNU Make 4.0+ (`make --version`) — ⚠️ **macOS ships 3.81, which cannot run the
  root `Makefile`.** Run `brew install make`, which installs it as `gmake`, then
  read every `make` below as `gmake`. Setting `alias make=gmake` in your shell is
  the least error-prone way to do that. The module Makefiles work with either
  version.
- [Flox](https://flox.dev/), optional, for the pinned toolchain. The environment
  is at the repository root and covers every module, GNU Make included.
- Docker (for local integration tests)
- A running Temporal server (for controller development)

### Clone and build

```bash
git clone https://github.com/NVIDIA/nke.git
cd nke
```

**Everything that builds publicly, in one command:**

```bash
make build   # builds agent, controller, nkectl, and helper binaries
```

The repository is a Go workspace, and `go.work` ties **every module** together.
`make build` compiles every one of them, so you do not need to build any module
from its own directory. `controller/` and `controller/nico-sim` used to be
outside the workspace because they had internal dependencies; #76
removed that dependency and they joined it.

⚠️ **After changing any `go.mod` or `go.sum`, run `make work-sync` and commit
`go.work` and `go.work.sum` with your change.** Nothing updates them for you,
and CI fails on a stale workspace. See [`docs/go-workspace.md`](docs/go-workspace.md).

Work on a single module from the root, so you never have to remember which
directory you are in:

```bash
make -C controller build test lint
make -C agent build test lint
```

Every module answers the same four verbs — `build`, `test`, `lint`, `fmt` — and
`make -C <module> help` lists the rest.

### Running tests

```bash
make test                      # every module's own suite
make -C controller test        # one module
make -C controller test-integration   # needs a running CWE deployment
```

`make test-integration` is the correct entry point for the integration suite. It
sits behind the `integration_test` build tag, so a plain `go test` in that
directory reports "build constraints exclude all Go files" and runs nothing.

On macOS the agent suite needs no special handling: `agent/Makefile` points
`TMPDIR` at `/private/tmp`, because the default sits under `/var`, which is a
symlink that the agent's own path checks reject.

### Before you open a pull request

```bash
make preflight
```

It lints and tests the modules you actually changed, and adds the workspace and
licence checks when your change touched what they cover. See
[`docs/development.md`](docs/development.md) for what it does not cover.

### Local development equals CI

The same tools, at the same versions, run locally and in CI.

**What.** Tool versions live in `make/versions.mk`, and the workflows read that
file rather than repeating a number. `make show-versions` prints the set. The
Flox environment at the repository root pins the same versions, and every target
runs from whatever is on your `PATH` if you would rather not use Flox.

**Why.** "Works on my machine" wastes a CI round trip to learn something a local
run could have told you. This holds only while your toolchain matches: a
golangci-lint older than the pin will silently miss findings that CI reports, so
compare `make show-versions` with what `make help` says you are actually running.

It does not hold everywhere, and `docs/development.md` lists the exceptions. The
largest is `govulncheck`, which CI runs under a different Go than the workspace
asks for.

## Code contributions

### Issue-first workflow

**Open an issue before submitting a PR** for any non-trivial change (new feature,
breaking change, significant refactor). This avoids duplicated effort and lets
maintainers give early feedback on the approach before you invest time coding.

For small bug fixes and documentation improvements, a PR without a prior issue is fine.

### Your first issue

1. Read the project's [README.md](https://github.com/NVIDIA/nke/blob/main/README.md)
    to learn how to set up the development environment.
2. Find an issue to work on. The best way is to look for the [good first issue](https://github.com/NVIDIA/nke/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
    or [help wanted](https://github.com/NVIDIA/nke/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22) labels.
3. Comment on the issue saying you are going to work on it.
4. Code! Make sure to update unit tests and documentation.
5. When done, [create your pull request](https://github.com/NVIDIA/nke/compare)
    referencing the issue with `Closes #<N>` in the description.
6. Verify that CI passes all [status checks](https://help.github.com/articles/about-status-checks/), or fix if needed.
7. Wait for maintainer review and address any feedback.
8. Once approved, a project maintainer will merge your pull request.

Remember, if you are unsure about anything, don't hesitate to comment on issues and ask for clarifications!

### Managing PR labels

Each PR must be labeled according to whether it is a "breaking" or "non-breaking" change (using GitHub labels). This is used to highlight changes that users should know about when upgrading.

For nke, a "breaking" change is one that modifies the public API in a non-backward-compatible way.
Backward-compatible changes (such as adding a new optional parameter) do not need to be labeled breaking.

Labels to apply:

| Label | When to use |
|-------|------------|
| `breaking` | Public API change that is not backward-compatible |
| `feature` | New capability added |
| `improvement` | Enhancement to existing functionality |
| `bug` | Regression or defect fix |
| `documentation` | Docs-only change |

### Seasoned developers

Once you have gotten your feet wet and are more comfortable with the code, you
can look at the prioritized issues for the next release in our [project board](https://github.com/orgs/NVIDIA/projects).

Look at the unassigned issues, and find an issue you are comfortable with
contributing to. Start with _Step 3_ from above, commenting on the issue to let
others know you are working on it. If you have any questions related to the
implementation of the issue, ask them in the issue instead of the PR.

### Branch naming

Branches should follow the pattern `<type>/<short-description>`:

| Prefix | When to use |
|--------|-------------|
| `feat/` | New feature |
| `fix/` | Bug fix |
| `docs/` | Documentation only |
| `refactor/` | Code restructuring without behavior change |
| `test/` | Adding or fixing tests |
| `chore/` | Maintenance, dependency updates, CI |

Example: `feat/add-retry-logic`, `fix/null-pointer-in-parser`

### Commit message format

nke uses [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <short description>

[optional body]

[optional footer: Closes #<issue>]
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`,
`ci`, `chore`, `revert`. The type and the scope are lower case.

Examples:
```
feat(api): add retry logic with exponential backoff
fix(parser): handle null pointer when input is empty
docs: update quickstart with Docker instructions
```

Breaking changes must include `BREAKING CHANGE:` in the footer or `!` after the type:
```
feat!: remove deprecated v1 API endpoints

BREAKING CHANGE: The /v1/foo endpoint has been removed. Use /v2/foo instead.
```

**The pull request title is what gets checked.** This repository squash-merges,
so the title becomes the one commit subject on `main`. The `pr-title` check
reports on it, and a rule on `main` rejects a subject that does not match. Fix a
rejected title by editing it; no push is needed. Nothing checks the commits on
your branch, because the squash discards them.

### Sign your commits (DCO)

Commits carry a `Signed-off-by:` trailer. Add one with `-s`:

```bash
git commit -s -m "fix(controller): handle an empty bundle list"
```

That appends a line matching the name and email in your Git configuration:

```
Signed-off-by: Jane Developer <jane@example.com>
```

The sign-off is your statement that you have the right to submit the work under
this repository's license. It is the [Developer Certificate of
Origin](https://developercertificate.org/), the same mechanism the Linux kernel
and Kubernetes use.

**Organization members are relaxed, not exempt.** The check skips a commit only
when the author is an NVIDIA organization member **and** GitHub reports the
commit as verified-signed. An unsigned commit from a member with no sign-off
still fails, reporting "Commit by organization member is not verified". Signing
your commits (`-S`) is what makes the exemption apply.

If you forget the `-s`, the simplest fix is to add it to every commit on the
branch:

```bash
git rebase --keep-base --signoff main
git push --force-with-lease
```

`--keep-base` matters: without it the rebase quietly moves your branch onto
whatever your local `main` points at, which on a fork is usually stale.

Remediation without rewriting history is also allowed, but it is not simply an
empty `-s` commit. The app looks for one exact sentence per unsigned commit,
each naming that commit's full SHA:

```
I, Jane Developer <jane@example.com>, hereby add my Signed-off-by to this commit: 3f53ada...

Signed-off-by: Jane Developer <jane@example.com>
```

The failing check prints the exact text to copy, so take it from there rather
than typing it by hand. Note the app itself advises against `--allow-empty` for
this, because an empty commit is dropped if anyone rebases the branch.

The configuration lives in [`.github/dco.yml`](.github/dco.yml).

## AI-assisted contributions

We welcome contributions written with the help of AI coding assistants (Claude,
Copilot, Cursor, etc.). When using AI assistance:

- **You are responsible** for reviewing and understanding all AI-generated code before
  submitting it. Do not submit code you cannot explain or defend in review.
- **Verify correctness:** AI-generated code may contain subtle bugs or security issues.
  Run the test suite and review the diff carefully.
- **No AI-generated sensitive content:** Do not use AI to generate security policies,
  legal text, or license headers — these require human judgment and legal review.
- **Disclose in the PR** if a substantial portion of the change was AI-generated,
  so reviewers can calibrate their review depth.

## Changelog

**Do not edit `CHANGELOG.md`.** It is generated from commit subjects by
[git-cliff](https://git-cliff.org), configured in `cliff.toml`, and a new
section is prepended when a release is cut.

Your commit subject is the changelog entry, so write it for a reader. The
conventional-commit type picks the section: `feat` → Added, `refactor` →
Changed, `fix` → Fixed, `perf` → Performance, `docs` → Documentation,
`ci`/`build`/`chore`/`test`/`style` → Maintenance, `revert` → Reverted. A
subject matching no type still appears, under Other. A breaking change — `!`
after the type, or a `BREAKING CHANGE:` footer — is marked in the entry.

This replaces the previous convention of adding an entry by hand in the same
pull request. That did not scale: every open pull request appended to the same
`### Added` list, so each conflicted with the next and whichever landed first
forced a rebase on all the others.

## Code of Conduct

All contributors are expected to follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## License headers

All new source files must include an SPDX license identifier as the first line:

```
# SPDX-License-Identifier: Apache-2.0
```

Adjust the comment style for your language (`//`, `/*`, `--`, etc.). This lets
license scanners identify the license without heuristic matching.

## Attribution

Portions adopted from https://github.com/NVIDIA-GitHub-Management/PLC-OSS-Template/blob/main/CONTRIBUTING.md
