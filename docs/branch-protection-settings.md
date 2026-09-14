# Branch protection settings

What `main` requires of a pull request, and why some checks block a merge while
others do not.

## What your pull request needs

| requirement | detail |
|---|---|
| `gate` green | the aggregate build check, see below |
| `DCO` green | every commit carries a `Signed-off-by` line, see `CONTRIBUTING.md` |
| signed commits | every commit must be signed |
| one approval | from a `CODEOWNERS` owner, and a push after approval dismisses it |
| conventional title | `type(scope): description`, see below |
| no merge commits | rebase your branch rather than merging `main` into it |

A maintainer presses Merge. Write access to the repository is not write access
to `main`, so a fully green and approved pull request still waits for one.

## Required and advisory checks

`gate` and `DCO` are the only two checks that block a merge. Everything else
that runs is advisory: a red advisory check does not stop a merge, and a missing
one is not a pass, so confirm the checks you expected actually ran.

`gate` is the aggregate job in
[`merge-gate.yml`](../.github/workflows/merge-gate.yml). It reports one result
for thirteen member workflows, which is why it is the only build check the
branch settings name. Naming the members individually would mean that renaming
or removing any one of them blocks every pull request until an admin
intervenes. For the same reason a member is never path-filtered: a check that
does not report cannot pass, and the pull request waits forever.
[The workflow guide](../.github/workflows/README.md) covers adding a member,
including the `gate.needs` edit that is easy to miss.

## Commit messages

The repository squash-merges only, so your pull request title becomes the commit
subject on `main` and has to match:

```
^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9/_.-]+\))?!?: .+
```

`feat(devx): add a thing` passes, while `Feat: add a thing` and
`feat(DevX): add a thing` do not. `git-cliff` turns the subject into the
changelog entry.

## Checking

```bash
gh pr checks <number> --repo dsx-ai-factory/nke --json name,workflow,state,bucket
gh api repos/dsx-ai-factory/nke/rules/branches/main --jq '[.[].type]'
```

If this page and the API disagree, the API is right.
