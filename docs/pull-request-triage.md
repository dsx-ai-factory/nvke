# Pull request triage

Triage is the first maintainer decision on a pull request: does it have an
issue, who reviews it, and, when its author is not trusted by copy-pr-bot,
whether to run CI. Issues have their own process in
[issue-triage.md](issue-triage.md).

## Every pull request links an issue

A pull request names the issue it resolves on the template's `Closes:` line, as
`Closes: #<N>`. A reviewer asks for an
issue before reviewing a pull request that has none, and the author opens one if
it does not exist yet. A pull request opened by automation, such as Dependabot or
the base-image digest bump, needs no issue.

A new feature or a change in behavior also needs the issue agreed before work
starts, as [GOVERNANCE.md](../GOVERNANCE.md#significant-changes) requires for
significant changes.

## Routing

Every pull request is routed by its paths, whoever wrote it:
[`.github/labeler.yml`](../.github/labeler.yml) sets its `area/*` labels, and
[`.github/CODEOWNERS`](../.github/CODEOWNERS) requests its reviewers. The
requested reviewers own the review.

## Before CI runs

CI on NVIDIA runners runs from a mirror branch, `pull-request/<N>`. copy-pr-bot
mirrors a head at once when its author is listed in
[`.github/copy-pr-bot.yaml`](../.github/copy-pr-bot.yaml). Any other author's
head waits for a vetter, inside NVIDIA or out, and however many pull requests
they have merged. [`test-gate-check.yml`](../.github/workflows/test-gate-check.yml)
labels what is waiting:

| Label | Meaning | Who clears it |
|---|---|---|
| `needs-ok-to-test` | copy-pr-bot has not mirrored this head, so the self-hosted checks have not run. | A vetter, by commenting `/ok to test <full SHA>` |
| `needs-workflow-approval` | GitHub is holding this head's `pull_request` workflows for a first-time contributor or outside collaborator. | A maintainer, by clicking **Approve workflows to run** |

`/ok to test` approves one commit. The code at that SHA runs on NVIDIA runners,
and every later push from the same author needs a new `/ok to test`.

### Who vets

The reviewers CODEOWNERS requests for a pull request vet it, and any maintainer
can. An area reviewer in [MAINTAINERS.md](../MAINTAINERS.md) who reviews a
pull request asks a maintainer to vet it.

### The vetting checklist

For each pull request carrying `needs-ok-to-test`:

1. Check that it links an issue. If not, ask for one before vetting.
2. Read the whole diff.
3. Check what CI will execute. A change under `.github/`, to a `Makefile`, or
   to a script under `hack/` or `build/` runs with the workflow's permissions,
   so review it line by line. Tests should use local servers, fakes and
   temporary directories, not the network or credentials.
4. Check the commits. With `needs-signed-commits` or `needs-dco`, the author
   must re-push, which makes a new head to vet. Ask for that first, and vet the
   new head.
5. Check the size against the issue. When a change is much larger than the
   issue asked for, ask for a smaller one before running CI.
6. Approve the held workflows, comment `/ok to test <full SHA>`, and tell the
   author what happens next.

## Cadence

The requested reviewers answer a new pull request within seven calendar days,
the same window as an issue's priority. For a head waiting on a vetter, that
means vetting it or saying what it needs first. Each area sweeps its pull
requests with its weekly issue sweep.

- Waiting on a vetter: [`is:open is:pr label:needs-ok-to-test`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-ok-to-test)
- One area, for example the node agent: [`is:open is:pr label:needs-ok-to-test label:area/agent`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-ok-to-test+label%3Aarea%2Fagent)
- Waiting on the author: [`is:open is:pr label:needs-signed-commits`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-signed-commits) and [`is:open is:pr label:needs-dco`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-dco)

## For contributors

Open or find the issue first, and link it from the pull request. You do not
need to do anything about `needs-ok-to-test` or `needs-workflow-approval`; a
maintainer clears them. `needs-signed-commits` and `needs-dco` are yours to fix,
as [CONTRIBUTING.md](../CONTRIBUTING.md#sign-your-commits-dco) describes.
Fixing them before review saves a second round of vetting.
