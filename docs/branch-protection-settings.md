# Branch protection settings

This document describes the branch protection rules that a repository admin
must configure in **Settings → Branches → Branch protection rules** (or
**Settings → Rules → Rulesets** for the newer ruleset UI).

These settings are not enforced automatically by files in the repository.
An admin must apply them once after cloning or during initial repo setup.

> ⚠️ **This table is the target state, not the live state.** It was written as
> a specification and has been read as a description. Measured on
> `NVIDIA/nke` on 2026-08-16, **four of these are not in effect**, including
> the one that matters most:
>
> ```
> $ gh api repos/NVIDIA/nke/branches/main/protection --jq .required_status_checks
> {"strict": false, "contexts": [], "checks": []}
> ```
>
> **No status check is required.** Every check this repository runs is
> advisory, so a red run can merge — and one did, sitting on `main` for
> fifteen hours before anyone noticed. Also not in effect: dismiss-stale
> approvals, Code Owner review, and require-branches-up-to-date.
>
> **Verify before quoting this table.** The `Live?` column below records what
> was measured; re-run the command above rather than trusting it.

## Rule: protect `main`

| Setting | Value | Live on `NVIDIA/nke`? | Reason |
|---|---|---|---|
| Require a pull request before merging | ✅ enabled | ✅ yes | No direct pushes to `main` |
| Required approvals | 1 | ✅ yes | At least one non-author review |
| Dismiss stale pull request approvals when new commits are pushed | ✅ enabled | 🔴 **no** | Re-review after force-push |
| Require review from Code Owners | ✅ enabled | 🔴 **no** | Enforces `.github/CODEOWNERS` |
| Require status checks to pass before merging | ✅ enabled | 🔴 **toggled on but EMPTY** — `contexts: []`, so nothing is required | CI must be green |
| Require branches to be up to date before merging | ✅ enabled | 🔴 **no** (`strict: false`) | No hidden conflicts |
| Restrict who can push to matching branches | ✅ enabled (nke-maintainer team) | ✅ yes | Prevents accidental force-push |
| Allow force pushes | ❌ disabled | ✅ correct | Preserve linear history |
| Allow deletions | ❌ disabled | ✅ correct | `main` cannot be deleted |

⚠️ **"Toggled on but empty" is worse than off.** The GitHub UI shows *Require
status checks* as enabled, so the setting reads as configured at a glance,
while the context list is empty and nothing actually blocks a merge.

## Recommended status checks

A required check is named after the **job**, not the workflow. These are the
checks the current workflows report.

> 🔴 **Read this before requiring anything. The table below is necessary but
> not sufficient, and acting on it alone will break pull requests.**
>
> **Two things have to be true** for a check to be safe to require: it must not
> be path-filtered (the table's column), **and** it must actually run on every
> pull request. Most of the checks below fail the second test.
>
> They live in workflows that trigger on `push` to `main` and
> `pull-request/[0-9]+`, not on the `pull_request` event — because self-hosted
> runners do not receive `pull_request`. They therefore appear only if
> `copy-pr-bot` mirrors the pull request head to a `pull-request/<N>` branch,
> which it gates on author vetting.
>
> **Measured 2026-08-16: 4 of 25 recent merged pull requests received none of
> them.** One was a 998-file, 68,000-line import that merged with no
> `golangci-lint`, no CodeQL, no `govulncheck`, no `gitleaks` and no `grype`.
> Its pull request page was green throughout, because the `pull_request`-
> triggered checks passed.
>
> **Requiring a check that a given pull request never receives leaves that pull
> request waiting forever.** Settle that first — see the "always reports"
> section below.
>
> ⚠️ **This list is also incomplete.** The repository reports far more checks
> than appear here, including `Analyze go`, `Analyze javascript-typescript`, `Analyze actions`,
> `grype`, `gitleaks`,
> `Check generated API code is up to date`, eight `govulncheck (<module>)`
> jobs, five `Unit tests (<module>)` jobs and five `Compile (<suite>)` jobs.
> Derive the live list rather than trusting this table:
>
> ```bash
> gh pr checks <number> --repo NVIDIA/nke --json name,workflow
> ```

| Check | Workflow | Safe to require? |
|---|---|---|
| `golangci-lint` | `.github/workflows/lint.yml` | yes |
| `golangci-lint (controller)` | `.github/workflows/lint.yml` | yes |
| `golangci-lint (nico-vms)` | `.github/workflows/lint.yml` | yes |
| `golangci-lint (nkectl)` | `.github/workflows/lint.yml` | yes |
| `Check THIRD_PARTY_NOTICES.md is up to date` | `.github/workflows/notices-check.yml` | yes |
| `build` | `.github/workflows/image.yml` | yes |
| `DCO` | the DCO GitHub App, not a workflow | yes |
| `actionlint` | `.github/workflows/actionlint.yml` | yes |
| `shellcheck` | `.github/workflows/shellcheck.yml` | yes |
| `Build binaries` | `.github/workflows/go-build.yml` | **no — path-filtered** |
| `Check go.work and go.work.sum are current` | `.github/workflows/go-work-check.yml` | **no — path-filtered** |

### Which workflows always report on a pull request

Only these seven trigger on the real `pull_request` event, so only these are
guaranteed to appear on every pull request regardless of who opened it:

`agent-test.yml` · `chart.yml` · `controller-test.yml` · `deb.yml` ·
`go-build.yml` · `go-work-check.yml` · `license.yml`

⚠️ **Every one of them is also path-filtered**, so a documentation-only pull
request receives none of them either — it gets `DCO` and nothing else.

Everything else — `lint`, `codeql`, `govulncheck`, `secrets`, `security-scan`,
`image`, `notices-check`, `actionlint`, `shellcheck`, `generate-check` — depends
on the mirror branch described above.

**So the honest position today is that no check is both unconditional and
meaningful.** Fixing that is a prerequisite for branch protection, not a
follow-up to it. The cheapest route is to move the checks that need no
self-hosted runner onto `pull_request`: `actionlint`, `shellcheck`,
`generate-check`, `codeql` and `govulncheck` already run on `ubuntu-latest`.

⚠️ **Do not mark the last two required.** Both filter on `paths:`, so a
documentation-only pull request never reports them — which is exactly the trap
described next. Require them only if the filters go, or a skip shim reports the
same check name.

⭐ **`actionlint` used to be in that group and was moved out deliberately.** Its
`paths:` filter was dropped so it reports on every push and can therefore be
required. The filter had already cost something: a red run merged to `main`, and
because every branch inherits the workflow file, the failure then reported on
every open pull request as though that pull request had caused it. Dropping the
filter was cheaper than the alternative — a skip shim reporting the same check
name — because the job is an eight-second static binary on a GitHub-hosted
runner, so it consumes no self-hosted capacity. **Add it to the required list;
it is not required yet.**

Confirm the exact names before you require one. A required check that names a
job which never reports leaves every pull request waiting forever:

```bash
gh pr view <number> --repo NVIDIA/nke \
  --json statusCheckRollup --jq '.statusCheckRollup[] | .name // .context'
```

The command below requires the first two and `DCO`. It leaves `build` out on
purpose: an image build is the slowest job here, and blocking every
documentation change on it buys little. Add it if you want that guarantee.

## Setting up via GitHub CLI

```bash
gh api repos/NVIDIA/nke/branches/main/protection \
  --method PUT \
  --field required_status_checks='{"strict":true,"contexts":["golangci-lint","actionlint","Check THIRD_PARTY_NOTICES.md is up to date","DCO"]}' \
  --field enforce_admins=true \
  --field required_pull_request_reviews='{"dismiss_stale_reviews":true,"require_code_owner_reviews":true,"required_approving_review_count":1}' \
  --field restrictions=null
```

> **Note:** The `restrictions` field controls push access. Set it to a
> specific team or users object if you want to lock down who can push
> even with admin privileges.
