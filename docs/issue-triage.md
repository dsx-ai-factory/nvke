# Issue and pull request triage

Triage is the first maintainer decision on an issue: is it actionable, where
does it belong, and how urgent is it. The decision is recorded as labels, so
anyone can see what is waiting and what was decided. A pull request needs
different first decisions: who reviews it, and, when its author is not trusted
by copy-pr-bot, whether to run CI; see [Pull requests](#pull-requests).

## The labels

| Label | Meaning | Who sets it |
|---|---|---|
| `needs-triage` | No maintainer has set a priority yet. Nothing for the reporter to do. | The issue forms, and automation on any open issue without a priority |
| `priority/P0` | Data loss, an unusable cluster or node, a security problem, or no workaround. Fix within 30 calendar days. | A triager |
| `priority/P1` | Significant impact with a workaround. Fix within 183 calendar days. | A triager |
| `priority/P2` | Everything else. No fix commitment. | A triager |
| `needs-info` | The reporter's turn: the issue cannot be triaged without more detail. | A triager |

Setting a priority is the act of triage. Automation removes `needs-triage`
when a `priority/*` label is present and puts it back when the last one is
removed. Nothing else clears it. A `needs-info` issue keeps `needs-triage`
until it has a priority.

When torn between P0 and P1, pick P1. Feature and documentation requests take
a priority too; most are P2.

The same labels and meanings are used by other NVIDIA repositories, so a
priority reads the same way across them.

## Who triages

Anyone with the Triage role or above on the repository can set these labels.
Maintainers and the area reviewers in [MAINTAINERS.md](../MAINTAINERS.md) are
expected to; see [GOVERNANCE.md](../GOVERNANCE.md). Engineering triages bugs,
Product triages feature requests, and maintainers triage documentation requests,
as [CONTRIBUTING.md](../CONTRIBUTING.md) says.

## The checklist

For each issue carrying `needs-triage`:

1. Confirm it is in scope and not a duplicate. Close a duplicate with a link to
   the original, and an out-of-scope request with a sentence saying why.
2. Check the kind label. The forms set `bug`, `feature` or `documentation`;
   an issue opened without a form may have none.
3. Check the area label. Automation sets one from the form's Component field
   where it can. Fix it if the reporter guessed wrong, and add one if it is
   missing.
4. Read the reporter's impact and the thread, then set one `priority/*` label.
   The impact dropdown is evidence, not the priority.
5. If the issue cannot be triaged yet, add `needs-info` and say what is
   missing instead of setting a priority.
6. Assign it, or leave it for a contributor and add `help wanted` or
   `good first issue`.

## Cadence

Every new issue should have a priority within seven calendar days. Each area
sweeps its queue once a week; the project lead sweeps the unrouted queue.

- Everything waiting: [`is:open is:issue label:needs-triage`](https://github.com/dsx-ai-factory/nvke/issues?q=is%3Aopen+is%3Aissue+label%3Aneeds-triage)
- One area, for example the NVKE controller: [`is:open is:issue label:needs-triage label:area/controller`](https://github.com/dsx-ai-factory/nvke/issues?q=is%3Aopen+is%3Aissue+label%3Aneeds-triage+label%3Aarea%2Fcontroller)
- Unrouted: [`is:open is:issue label:needs-triage -label:area/controller -label:area/agent -label:area/cli -label:area/chart -label:area/os-image -label:area/ci-build -label:area/docs -label:area/deps -label:area/nktp -label:area/generated`](https://github.com/dsx-ai-factory/nvke/issues?q=is%3Aopen+is%3Aissue+label%3Aneeds-triage+-label%3Aarea%2Fcontroller+-label%3Aarea%2Fagent+-label%3Aarea%2Fcli+-label%3Aarea%2Fchart+-label%3Aarea%2Fos-image+-label%3Aarea%2Fci-build+-label%3Aarea%2Fdocs+-label%3Aarea%2Fdeps+-label%3Aarea%2Fnktp+-label%3Aarea%2Fgenerated)
- Waiting on the reporter: [`is:open is:issue label:needs-info`](https://github.com/dsx-ai-factory/nvke/issues?q=is%3Aopen+is%3Aissue+label%3Aneeds-info)

## For reporters

You do not need to do anything about `needs-triage`. To argue for a higher
priority, comment with the impact: what is broken, how many clusters or nodes
it affects, and whether a workaround exists. Triage reads the thread before
changing a priority that is already set.

## What the automation does

[`issue-triage-check.yml`](../.github/workflows/issue-triage-check.yml) runs on
issue events and once an hour:

- adds `needs-triage` to any open issue with no `priority/*` label, and removes
  it once one is set;
- on a new issue, maps the form's Component field to an `area/*` label, once,
  and never puts back an area a triager removed.

Pull requests carry none of these labels.

## Pull requests

Every pull request is routed by its paths, whoever wrote it:
[`.github/labeler.yml`](../.github/labeler.yml) sets its `area/*` labels, and
[`.github/CODEOWNERS`](../.github/CODEOWNERS) requests its reviewers. The
requested reviewers own the review.

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

1. Read the whole diff.
2. Check what CI will execute. A change under `.github/`, to a `Makefile`, or
   to a script under `hack/` or `build/` runs with the workflow's permissions,
   so review it line by line. Tests should use local servers, fakes and
   temporary directories, not the network or credentials.
3. Check the commits. With `needs-signed-commits` or `needs-dco`, the author
   must re-push, which makes a new head to vet. Ask for that first, and vet the
   new head.
4. Check the issue. A bug fix says what it fixes. A new feature or a change in
   behavior needs an agreed issue first, as
   [GOVERNANCE.md](../GOVERNANCE.md#significant-changes) requires. If the
   approach is not agreed, settle it on the issue before running CI.
5. Check the size against the problem. When a change is much larger than the
   issue asked for, ask for a smaller one before running CI.
6. Approve the held workflows, comment `/ok to test <full SHA>`, and tell the
   author what happens next.

### Cadence

The requested reviewers answer a new pull request within three business days.
For a head waiting on a vetter, that means vetting it or saying what it needs
first. Each area sweeps its pull requests with its weekly issue sweep.

- Waiting on a vetter: [`is:open is:pr label:needs-ok-to-test`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-ok-to-test)
- One area, for example the node agent: [`is:open is:pr label:needs-ok-to-test label:area/agent`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-ok-to-test+label%3Aarea%2Fagent)
- Waiting on the author: [`is:open is:pr label:needs-signed-commits`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-signed-commits) and [`is:open is:pr label:needs-dco`](https://github.com/dsx-ai-factory/nvke/pulls?q=is%3Aopen+is%3Apr+label%3Aneeds-dco)

### For contributors

You do not need to do anything about `needs-ok-to-test` or
`needs-workflow-approval`; a maintainer clears them. `needs-signed-commits` and
`needs-dco` are yours to fix, as [CONTRIBUTING.md](../CONTRIBUTING.md#sign-your-commits-dco)
describes. Fixing them before review saves a second round of vetting.
