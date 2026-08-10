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

- Go 1.24+ (`go version`)
- [Flox](https://flox.dev/) for reproducible toolchains inside `controller/`
- Docker (for local integration tests)
- A running Temporal server (for controller development)

### Clone and build

```bash
git clone https://github.com/NVIDIA/nke.git
cd nke
```

**Controller (cloud-workflow-engine):**

```bash
cd controller
flox activate
make build   # builds ./bin/cloud-workflow-engine
make test    # unit tests with coverage
make lint    # golangci-lint
```

**Agent (nke-agent):**

```bash
cd agent
make build   # builds for amd64 and arm64
make test    # unit tests
make lint    # golangci-lint
```

### Running tests

```bash
# Unit tests (no external dependencies):
cd controller && flox activate -- make test
cd agent && make test

# Integration tests (requires a running CWE deployment):
cd controller/integration-test
CWE_API_URL=http://localhost:8080/api go test -v -timeout=3h
```

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

Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`, `perf`

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
