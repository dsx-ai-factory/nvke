# Issue triage

Triage is the first maintainer decision on an issue: is it actionable, where
does it belong, and how urgent is it. The decision is recorded as labels, so
anyone can see what is waiting and what was decided.

## The labels

| Label | Meaning | Who sets it |
|---|---|---|
| `needs-triage` | The triagers' turn: no priority yet, and not waiting on the reporter. | The issue forms, and automation on any open issue without a priority or `needs-info` |
| `priority/P0` | Data loss, an unusable cluster or node, a security problem, or no workaround. Fix within 30 calendar days. | A triager |
| `priority/P1` | Significant impact with a workaround. Fix within 183 calendar days. | A triager |
| `priority/P2` | Everything else. No fix commitment. | A triager |
| `needs-info` | The reporter's turn: the issue cannot be triaged without more detail. | A triager; automation removes it when the reporter comments |

Setting a priority is the act of triage. Automation removes `needs-triage`
while an issue has a `priority/*` or `needs-info` label, and puts it back when
it has neither. When the reporter comments on a `needs-info` issue, automation
removes `needs-info`; without a priority, the issue returns to the triage
queue. A `needs-info` issue with no reply gets a reminder after 14 days and is
closed after 30; the reporter's next comment reopens it.

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

You do not need to do anything about `needs-triage`. If your issue gets
`needs-info`, reply with what was asked for; your comment puts it back in the
triage queue. Without a reply it closes after 30 days, and a comment with the
details reopens it. To argue for a higher
priority, comment with the impact: what is broken, how many clusters or nodes
it affects, and whether a workaround exists. Triage reads the thread before
changing a priority that is already set.

## What the automation does

[`issue-triage-check.yml`](../.github/workflows/issue-triage-check.yml) runs on
issue events, on the reporter's comments, and once an hour:

- adds `needs-triage` to any open issue with no `priority/*` or `needs-info`
  label, and removes it once either is set;
- removes `needs-info` when the reporter comments, or on the next hourly sweep
  if that run failed; an issue with no priority then returns to `needs-triage`;
- reminds the reporter 14 days after `needs-info` was added, closes the issue
  as not planned at 30 days, and reopens it if the reporter then comments;
- on a new issue, maps the form's Component field to an `area/*` label, once,
  and never puts back an area a triager removed.

Pull requests carry none of these labels. They are routed by the path-based
labeler and reviewed through CODEOWNERS.
