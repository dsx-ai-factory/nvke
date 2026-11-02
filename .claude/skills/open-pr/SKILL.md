---
name: open-pr
description: Open or update a pull request on dsx-ai-factory/nvke following the repository's fork, squash-merge, DCO, labeling, and CI-presence rules. Use when asked to open a PR, push a branch for review, or check why PR checks look wrong.
disable-model-invocation: true
---

# Open a pull request

Follow [CONTRIBUTING.md](../../../CONTRIBUTING.md) — this skill is the
executable checklist, not a replacement for it.

## Before pushing

1. Branch name is `<type>/<short-description>` (`feat/`, `fix/`, `docs/`,
   `refactor/`, `test/`, `chore/`). Push it to your fork, never to
   `dsx-ai-factory/nvke`, even with write access, and open the PR from there:
   `gh pr create --repo dsx-ai-factory/nvke --head <your-user>:<branch>`.
2. Run `make preflight` from the repository root. It lints and tests the
   modules you changed and adds workspace and license checks when relevant.
3. Every commit carries a DCO sign-off (`git commit -s`). Missing one on an
   existing branch: `git rebase --keep-base --signoff main` then
   `git push --force-with-lease`. Organization members are relaxed only when
   the commit is also verified-signed (`-S`) — relaxed, not exempt.

## Opening the PR

4. **The PR title is the commit.** This repository squash-merges, so the title
   becomes the one commit subject on `main` and is what the `pr-title` check
   and the branch rule validate. It must be a Conventional Commit
   (`type(scope): description`, lower case). Branch commits are discarded by
   the squash; do not polish them.
5. Reference the issue with `Closes #<N>` for non-trivial changes (the repo is
   issue-first — open one before a significant PR).
6. Apply exactly one change-category label: `feature`, `bug`, or
   `documentation`. There is no `breaking` label; mark a breaking change
   with `!` in the title or a `BREAKING CHANGE:` footer.
7. If a substantial portion of the change was AI-generated, disclose that in
   the PR description (required by CONTRIBUTING.md). Do not add agent
   attribution: no `Co-Authored-By` trailer naming an agent in any commit, and
   no "Generated with …" footer in the PR description.

## After opening — verify checks are PRESENT, not just green

Most checks build through a `pull-request/<number>` branch created by
copy-pr-bot, which gates on **author vetting**. For unvetted authors those
checks are never created, and **their absence looks identical to success**: a
column of green ticks with checks simply missing.

```bash
gh pr checks <number> --repo dsx-ai-factory/nvke --json name,workflow
```

Which checks block a merge is a repository setting. Read it with
`gh api repos/dsx-ai-factory/nvke/rules/branches/main --jq '[.[].type]'`. `gate` and DCO always report; golangci-lint,
CodeQL, govulncheck, gitleaks, grype, actionlint, shellcheck, the image build,
and the notices gate depend on the mirror and are legitimately absent for
unvetted authors — flag that state rather than treating it as passing.
