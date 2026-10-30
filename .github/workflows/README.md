# NVKE workflow design guide

This directory has two kinds of CI workflow: required pull-request checks and
advisory or release workflows. Keep their event ownership separate. Most CI
duplication and missing-check failures in this repository came from one workflow
trying to own both kinds of event.

The two required branch-protection contexts are `gate` and `DCO`. The `gate`
job is the stable aggregate produced by [merge-gate.yml](merge-gate.yml). `DCO`
is a GitHub App. Do not make an individual workflow job a required context.

## Invariants

Preserve these rules when adding or changing a workflow:

1. The aggregate owns required pull-request coverage. A required member is
   called once by `merge-gate.yml`; it does not also trigger on `pull_request`,
   `pull-request/N`, or `dependabot/**`.
2. Member workflows own post-merge coverage. They retain a direct `push` trigger
   on `main`, with one authoritative path filter where the check is scoped.
3. Pull requests run the full aggregate. A called workflow ignores its own
   `paths:` filter. Do not copy member path filters into the aggregate.
4. Post-merge `main` runs are canaries selected by member path filters. They are
   not a second full gate.
5. A required member must succeed on a GitHub-hosted runner with the permissions
   available to a fork pull request and no required repository secrets. Work
   that needs NVIDIA runners, privileged infrastructure, or required secrets
   stays advisory.
6. `pull-request/N` is a bot-owned transport branch, not a pull-request base.
7. A base retarget must re-run the gate. Keep `edited` in the aggregate's
   `pull_request.types` list.
8. Concurrency must not let one `main` commit erase another commit's result.
9. Every tool download retries. A pinned release tarball is fetched with
   `--retry 8 --retry-all-errors --retry-max-time 240`. Retries default to 0, so
   a download with no `--retry` fails the job on the first bad response -- which
   a release-CDN 504 has done. `--retry` alone covers curl's transient set (408,
   429, 500, 502, 503, 504, 522, 524 and timeouts); `--retry-all-errors` extends
   that to the rest, such as a connection reset part way through a transfer.
   Pair it with `-o`, never a pipe: curl cannot rewind output already written to
   a pipe, so a retry there duplicates data. Prefer an existing `setup-*`
   composite action over inlining a new download.

## Event ownership

| Event | Owner | Expected coverage |
|---|---|---|
| Pull request, including a stacked PR | `merge-gate.yml` | Every required member |
| Pull-request base retarget | `merge-gate.yml` through `edited` | Every required member against the new base |
| PR accidentally based on `pull-request/N` | Excluded | Mirror branches are bot-owned transport, not review bases |
| Vetted `pull-request/N` mirror push | Advisory workflows | Self-hosted, security, image, notices, and backend E2E checks as applicable |
| Dependabot pull request | `merge-gate.yml` plus relevant advisory workflows | Required members once; advisory checks separately |
| Push to `main` | Individual workflows | Checks selected by their direct triggers and path filters |
| Release tag | Release and publishing workflows | Release-specific build, scan, license, and publish gates |
| Schedule or manual dispatch | The workflow declaring it | Periodic or operator-requested work |

The aggregate excludes pull requests whose base matches `pull-request/[0-9]+`.
Opening a PR against a mirror branch is accidental and would otherwise spend a
full gate on bot transport state.

GitHub groups title, body, and base changes under the `edited` activity. There
is no base-only trigger subtype, so an ordinary metadata edit also re-runs the
gate. That cost is intentional: omitting `edited` lets an unchanged head retain
a green result evaluated against its old base.

Trigger changes do not enqueue a run for every already-open PR. In particular,
an existing stacked PR may still carry the workflow from its current merge ref.
When changing the aggregate trigger, state the rollout boundary in the PR and
verify existing stacks rather than claiming immediate retroactive coverage.

A member's post-merge `main` run is a canary nobody is watching, so
`main-health-check.yml` sweeps those runs every half hour and files one tracking
issue while any member is red. It reads the member list out of `merge-gate.yml`,
so a member added or removed there needs no edit to it. A red `main` blocks
every pull request whose merge ref contains the failing commit: `gate` is
required and each PR builds against its own merge ref. A stack based on a
branch that predates the failure is unaffected until it rebases.

## Adding a required gate member

First confirm that the job belongs in the required gate:

- It runs safely for forks and unvetted contributors.
- It needs no required secret and no NVIDIA self-hosted runner.
- Its latency is acceptable on every pull request.
- Failure should block a merge.
- It carries no job-level `if:` other than the repository guard (see
  "Repository guards" below), because `gate` requires `success` from every
  member and a skipped member fails it.

If any answer is no, add an advisory workflow instead.

`nvkectl-bootstrap-e2e.yml` is the worked example of a no, and its header
records which answers fail and what would have to change first. Read that
before proposing it, or any other e2e lane, as a member again.

### 1. Make the workflow reusable

A typical required member owns `main` directly and is called for pull requests:

```yaml
name: Example Check

on:
  push:
    branches: [main]
    paths:
      - 'area/**'
      - '.github/workflows/example-check.yml'
  workflow_call:
  workflow_dispatch: {}

permissions: {}

concurrency:
  group: Example Check-${{ github.ref }}-${{ github.ref == 'refs/heads/main' && github.sha || '' }}
  cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}

jobs:
  check:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      # ...
```

Do not add any of these to a required member:

```yaml
on:
  pull_request:
  push:
    branches:
      - "pull-request/[0-9]+"
      - "dependabot/**"
```

Those events already reach the member through the aggregate. Naming them again
runs the same check twice.

`workflow_dispatch` and a schedule are fine when the check also has a manual or
periodic use. Preserve release-tag triggers when extending an existing workflow.

### 2. Choose the complete `main` path filter

The filter controls only the member's direct post-merge run. Include:

- every module or directory the job actually checks;
- shared inputs such as `go.work`, `go.work.sum`, or `Makefile`
  where they can change the result; and
- the workflow file itself.

Compare the filter with the job matrix, not only the workflow name. For example,
`controller-test.yml` also tests `cli`, so its filter includes `cli/**` even
though the workflow is named for the controller.

Use no path filter for a universal repository check. Do not duplicate a member
filter in `merge-gate.yml`: reusable workflow calls do not apply it, and two
copies drift independently.

### 3. Add the call and aggregate dependency

Add a reusable-workflow job to `merge-gate.yml` with the least permissions it
needs:

```yaml
jobs:
  example:
    uses: ./.github/workflows/example-check.yml
    permissions:
      contents: read
```

Then add the job ID to `gate.needs`. Missing that second edit is fail-open: the
work may run, but its failure will not affect the required `gate` result.

Keep `gate.name` equal to `gate`, keep `if: always()`, and do not rename the
required context. The final job requires every member to report `success`.
A `skipped` member fails the gate, so a member carries no job-level `if:`
other than the repository guard. A job inside a member that must sometimes
skip, such as the `report` job in `coverage.yml`, sits beside a job that
always runs, so the called workflow as a whole still concludes `success`.

A caller cannot grant less permission than a called workflow requests. Declare
the minimum at both levels. Optional secrets are acceptable only when the member
still succeeds without them on an untrusted pull request.

## Adding an advisory workflow

Advisory workflows are appropriate for jobs that are slow, privileged,
infrastructure-dependent, secret-dependent, or useful only for publishing.

- A GitHub-hosted, secret-free advisory job may use `pull_request` directly.
- A job that needs an NVIDIA runner or trusted credentials uses vetted
  `pull-request/N` pushes. Add `dependabot/**` only when the check is relevant
  and safe for that trusted repository branch.
- Keep backend E2E and similar expensive checks path-filtered.
- Never add an author-vetted mirror check to branch protection: unvetted authors
  do not receive it, so the required context would remain absent forever.
- Tag and publishing workflows should remain separate from pull-request event
  ownership.

The absence of an advisory check is not a pass. When reviewing a PR, verify that
the checks expected for that author, path set, and backend are present.

## Concurrency

Use a literal workflow-specific group name. Inside a reusable workflow,
`${{ github.workflow }}` resolves to the caller's name. If every member uses it,
siblings called by `merge-gate.yml` compute the same group and cancel one
another.

Include the SHA for `main`:

```yaml
group: Example Check-${{ github.ref }}-${{ github.ref == 'refs/heads/main' && github.sha || '' }}
cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}
```

GitHub retains at most one running and one pending entry in a concurrency group.
Even with `cancel-in-progress: false`, a third push replaces the older pending
entry. A SHA-scoped `main` group prevents back-to-back merges from losing a
result. Pull-request refs should remain shared so a new update cancels stale
work.

The e2e lanes pay for the SHA-scoped groups on a busy day: GitHub never cancels
a queued leg whose commit has been superseded, so two-hour legs pile up for
commits that are no longer the head. `e2e-queue-sweeper.yml` runs every ten
minutes, keeps every running leg and the newest two queued runs per lane, and
cancels the older queued ones. Dispatch a lane by hand on a swept commit when
its result is wanted. The same sweep cancels queued lane runs on a
`pull-request/<n>` mirror branch once that pull request is merged or closed:
their result changes nothing, and they share the arm64 pool with main.

Release gates need separate treatment: do not cancel a tag run, and keep tag
groups distinct. `license.yml` documents the release-specific version of this
rule.

## Runner and trust boundary

The required gate runs on the real `pull_request` event. Its members use
GitHub-hosted runners because the NVIDIA pool is not available to every fork or
unvetted contributor.

Mirror pushes are created only for vetted authors. They are the trust boundary
for advisory jobs using NVIDIA runners or credentials. Do not move one of those
jobs into the required gate merely to make it visible in branch protection.
Either make it safe and self-contained on a GitHub-hosted runner or leave it
advisory.

Start workflows and jobs with no permissions, then grant only what they use:

```yaml
permissions: {}

jobs:
  check:
    permissions:
      contents: read
```

Document any exception next to the call. For example, `govulncheck` needs
`actions: read` and `security-events: write` to upload SARIF.

## Repository guards

A job that must not run in a fork starts with the repository guard:

```yaml
    if: github.repository_id == '1323115536'
```

`1323115536` is the id of `dsx-ai-factory/nvke`, which
`gh api repos/dsx-ai-factory/nvke --jq .id` prints. The guard compares the id
and never the slug, because the id survives a rename or a transfer and the
slug does not. When the guards compared the slug, moving the repository turned
every one of them false, every gate member skipped, and `gate` passed with
nothing run, because it tolerated `skipped` at the time. The gate now requires
`success`, so the same event would fail every pull request instead of passing
it, and `make lint-workflows` rejects a workflow that compares
`github.repository` to a string.

In a compound condition the guard stays the first clause. In a fork every
guarded job skips and `gate` fails, which is the intended reading: nothing was
checked there. Values that should follow the repository name, such as the
`org.opencontainers.image.source` annotation or a `gh api repos/...` call,
keep using `${{ github.repository }}`, which GitHub fills in with the current
slug.

## Review checklist

Before submitting a workflow change:

1. Decide whether it is required, advisory, release-only, scheduled, or manual.
2. Confirm exactly one owner for each event; search for the workflow name and
   its branch patterns across this directory.
3. For a required member, add `workflow_call`, a direct `main` trigger, the call
   in `merge-gate.yml`, and its ID in `gate.needs`.
4. Compare every `paths:` list with the job matrix and shared inputs.
5. Confirm the job succeeds on its chosen runner and trust boundary.
6. Give it a literal concurrency group and protect every `main` SHA.
7. Keep permissions minimal and required secrets out of the aggregate.
8. Preserve the aggregate's stacked-PR, mirror-base, and `edited` behavior.
9. Update this guide if the ownership model changes.
10. Guard a job that must not run in a fork with `github.repository_id`, never
    with the slug.
11. Give every `curl` that fetches a tool the retry flags from invariant 9,
    and keep the committed-digest check that follows it.

Run the static checks from the repository root:

```bash
# Runs actionlint, and first checks it is the version CI pins.
make lint-workflows

# Not covered by that target:
for workflow in .github/workflows/*.yml; do
  yq eval '.' "${workflow}" >/dev/null
done
git diff --check
```

After pushing, inspect the PR rather than assuming green means complete:

```bash
gh pr checks <number> --repo dsx-ai-factory/nvke --json name,workflow,state,bucket
```

For a required member, confirm it appears inside `Merge gate`, no standalone
copy runs from the PR or mirror branch, and the final `gate` context reports.
For an advisory workflow, confirm its expected trigger actually produced a
check; an absent advisory check does not look like a failure.

What `main` requires of a pull request is a repository setting. Read it with
`gh api repos/dsx-ai-factory/nvke/rules/branches/main --jq '[.[].type]'`.
