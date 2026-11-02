# Contributing to NVKE

If you are interested in contributing to NVKE, your contributions will fall
into three categories:
1. You want to report a bug, feature request, or documentation issue
    - File an [issue](https://github.com/dsx-ai-factory/nvke/issues/new/choose)
    describing what you encountered or what you want to see changed.
    - For usage questions, use [GitHub Discussions Q&A](https://github.com/dsx-ai-factory/nvke/discussions/categories/q-a)
    instead of an issue.
    - Maintainers triage issues as [docs/issue-triage.md](docs/issue-triage.md)
    describes. Engineering reviews bugs, Product reviews feature requests, and
    maintainers review documentation requests. The impact reported in an issue
    is evidence for triage, not the `priority/P0` to `priority/P2` label a
    triager sets. If you believe an issue needs prompt attention, comment on it
    to notify the team.
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

- Go — the floor is the `go` directive in `go.work`, not any single module's
  `go.mod`, and this page deliberately does not repeat the number. Read it off
  the workspace: `awk '/^go /{print $2; exit}' go.work`. A newer Go is fine:
  `make` pins `GOTOOLCHAIN` to that same value, because `strcase` ships Unicode
  tables generated per Go release and panics at startup when they do not match
  the standard library's `unicode.Version`. Pinning keeps the two in step on a
  Go release it has no tables for. Set `GOTOOLCHAIN` yourself to override that.
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
git clone https://github.com/dsx-ai-factory/nvke.git
cd nvke
```

To contribute, clone your fork instead. See [Work from a fork](#work-from-a-fork).

**Everything that builds publicly, in one command:**

```bash
make build   # builds agent, controller, nvkectl, and helper binaries
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

1. Read the project's [README.md](https://github.com/dsx-ai-factory/nvke/blob/main/README.md)
    to learn how to set up the development environment.
2. Find an issue to work on. The best way is to look for the [good first issue](https://github.com/dsx-ai-factory/nvke/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
    or [help wanted](https://github.com/dsx-ai-factory/nvke/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22) labels.
3. Comment on the issue saying you are going to work on it.
4. Code! Make sure to update unit tests and documentation.
5. When done, [create your pull request](https://github.com/dsx-ai-factory/nvke/compare)
    referencing the issue with `Closes #<N>` in the description.
6. Verify that CI passes all [status checks](https://help.github.com/articles/about-status-checks/), or fix if needed.
7. Wait for maintainer review and address any feedback.
8. Once approved, a project maintainer will merge your pull request.

Remember, if you are unsure about anything, don't hesitate to comment on issues and ask for clarifications!

### Managing PR labels

Each PR must mark whether it is a breaking change. This is used to highlight changes that users should know about when upgrading.

For NVKE, a "breaking" change is one that modifies the public API in a non-backward-compatible way.
Backward-compatible changes (such as adding a new optional parameter) are not breaking.
Mark a breaking change with `!` after the type in the PR title or a `BREAKING CHANGE:` footer (see [Commit message format](#commit-message-format)); there is no label for it.

Apply one change-category label:

| Label | When to use |
|-------|------------|
| `feature` | New capability, or an improvement to existing functionality |
| `bug` | Regression or defect fix |
| `documentation` | Docs-only change |

### Qualification results

A Qualify e2e run, started by one of the `qualify-e2e` labels described in
[Running one](docs/qualification/overview.md#running-one), shows its test
results on the run page in a `results` card below the `plan` card. The card has
one row per stream and lists each stream's specs, with the failure messages,
read from that stream's JUnit report. A dry run has no `results` card.

### Seasoned developers

Once you have gotten your feet wet and are more comfortable with the code, you
can look at the prioritized issues for the next release in our [project board](https://github.com/orgs/dsx-ai-factory/projects).

Look at the unassigned issues, and find an issue you are comfortable with
contributing to. Start with _Step 3_ from above, commenting on the issue to let
others know you are working on it. If you have any questions related to the
implementation of the issue, ask them in the issue instead of the PR.

### Work from a fork

Push your branches to your own fork and open pull requests from there, not
from a branch on `dsx-ai-factory/nvke`. This applies even if you have write
access to the repository.

```bash
gh repo fork dsx-ai-factory/nvke --clone
```

That creates the fork, clones it as `origin` and adds this repository as
`upstream`. In an existing clone of this repository, `gh repo fork --remote`
does the same: your fork becomes `origin` and the old `origin` becomes
`upstream`.

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

NVKE uses [Conventional Commits](https://www.conventionalcommits.org/):

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

We require that all contributors "sign-off" on their commits. This certifies that
the contribution is your original work, or you have rights to submit it under the
same license, or a compatible license.

Any contribution which contains commits that are not Signed-Off will not be
accepted.

To sign off on a commit, use the `--signoff` (or `-s`) option when committing
your changes:

```bash
git commit -s -m "fix(controller): handle an empty bundle list"
```

This appends the following to your commit message, matching the name and email in
your Git configuration:

```
Signed-off-by: Jane Developer <jane@example.com>
```

This applies to every contributor, including NVIDIA organization members. There
is no exemption for verified or signed commits.

#### Developer Certificate of Origin

The sign-off is the [Developer Certificate of
Origin](https://developercertificate.org/) (DCO), the same mechanism the Linux
kernel and Kubernetes use. Its full text:

```text
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

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

### Verified signatures

The sign-off above is a line of text in the commit message. `main` also
requires a cryptographic signature on every commit. The ruleset reports it in
the merge box as "Commits must have verified signatures." and blocks the merge
until each commit on the branch carries one. An open pull request in that
state gets the `needs-signed-commits` label.

Sign with a GPG or SSH key that is registered on your GitHub account, under an
email address the account has verified. GitHub's guide to [commit signature
verification](https://docs.github.com/en/authentication/managing-commit-signature-verification)
covers both key types. Then sign every commit in this clone by default:

```bash
git config commit.gpgsign true
```

To sign the commits already on a branch, replay them with a signature:

```bash
git rebase --keep-base --force-rebase --gpg-sign main
git push --force-with-lease
```

`--force-rebase` matters: without it the rebase keeps the commits that already
sit on the base as they are, unsigned. `--gpg-sign` covers SSH keys too; the
option is older than SSH signing. For a single commit, `git commit --amend
--no-edit -S` followed by the same push does the same.

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
  so reviewers can calibrate their review depth. Write the note yourself.
- **No agent attribution:** Do not add `Co-Authored-By` trailers naming an AI
  assistant to commits, or tool-generated footers such as "Generated with …" to
  PR descriptions. You are the author of the change; turn off your tool's
  default attribution.

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
