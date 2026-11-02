# Architecture decision record conventions

**Status:** accepted
**Date:** 2026-10-14

## Context

This is based on a proposal by Stefan Schimanski and Phillip Wittrock for a similarly large codebase with many developers and teams collaborating across the globe where synchronous communication is not always possible.

An ADR is how that decision survives for someone who was not in the room. It records what was decided and why, next to the code it governs.

## Decision

We will record significant architectural decisions in version-controlled Markdown ADRs, using the scope, placement, and lifecycle conventions below.

### When to write one

Write one when the choice affects structure, an interface, a dependency, or how a component is built, and reversing it later without a record would mislead the next change. One decision per file. One or two pages.

Routine bugfixes and refactors that preserve architectural boundaries do not need an ADR. An ADR is not an implementation plan, a rollout, or a release checklist.

### Relationship to design documents

[`docs/design/`](../design/README.md) holds feature and system proposals and detailed design records, including API behavior, data flows, and implementation approaches. Existing component design directories, such as `docs/nktp/designs/`, continue to serve that purpose.

When a design introduces a significant architectural choice described above, record that choice and its rationale in an ADR in the [owning directory](#where-it-goes). Link the design for detail instead of duplicating it. Proposed architectural decisions belong in ADRs with status `proposed`. A design without such a choice does not need a separate ADR.

Keep existing design records and narrative documents, including [`docs/architecture.md`](../architecture.md), in place and link them as context. Do not migrate them or require retrospective ADRs solely to adopt this guidance. When a new ADR changes an existing design, update that design to link to the decision.

### What goes in the record

Include the context a later reader can check against the tree, the decision in the form "We will", the consequences, and the options that were actually in contention.

Leave out anything the reader cannot open: Jira, Slack, Confluence, internal GitLab, and unreleased names used as the reason. Leave out a roster of who was consulted. Leave out the implementation steps.

### Where it goes

Put it in the top-level component that owns the decision, in `<component>/docs/adrs/`. Create that directory with the first ADR. Do not add empty directories ahead of it.

- `controller/docs/adrs/` for the controller. The directory is `controller/`, singular. A subdirectory such as `controller/nico-sim/` lives here unless the decision binds another top-level component.
- `management-engine/docs/adrs/` for the cluster service.
- The same rule for `agent/`, `cluster-api-provider-provisioned/`, `cli/`, `os-image/`, and `release/`.

Put it in this directory when it binds more than one component, or when it is about the repository as a whole.

If a decision starts in one component and later binds another, leave the original file and supersede it with a new record here. Do not move the file.

### Workflow

Create the file in that directory, with status `proposed`, and open the pull request. When it is accepted, change that status line to `accepted` in the same file. The file is not moved.

### Filename

Name it `YYYYMMDD-kebab-title.md` on the day you create it. That date matches the Date line. Do not rename the file when the status changes.

A second record on the same day uses a different slug. If that slug is taken, append a digit with no extra hyphen: `20261014-node-join2.md` sorts after `20261014-node-join.md`. The heading is the title. Cite the path, because the same date can exist in more than one directory.

### Status

- `proposed` — not binding, even if the file is merged.
- `accepted` — the decision. An architectural decision follows the significant-change rule in [GOVERNANCE.md](../../GOVERNANCE.md).
- `rejected` — keep the file.
- `superseded by <relative path>` — the later record replaces it. Keep the old file. The only edit after acceptance is this line.

## Consequences

Decision rationale and status remain reviewable alongside the code. Component ownership determines where contributors find a record. Designs and ADRs remain separate, so contributors must maintain links between them. Accepted decisions stay as history and are superseded rather than rewritten.

## Options considered

- **Dated ADRs beside the owning component (chosen):** preserve individual decisions and their rationale without duplicating detailed designs.
- **Design records alone:** avoid a separate record, but mix individual architectural decisions and their status with evolving design detail.
- **Sequentially numbered ADRs:** provide a simple order, but concurrent pull requests can choose the same next number without a Git conflict.

## Template

```markdown
# Short noun phrase

**Status:** proposed
**Date:** YYYY-MM-DD

## Context

What is forcing a choice, in terms a later reader can check against the tree.

## Decision

We will ...

## Consequences

What gets easier, what gets harder, and what this gives up.

## Options considered

- **This decision:** one sentence on why it won.
- **The serious alternative:** one sentence on why it lost.
```
