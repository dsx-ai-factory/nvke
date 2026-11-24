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
3. Every commit carries a DCO sign-off (`git commit -s`). There is no
   exemption, for organization members or for signed commits. Missing one on
   an existing branch: `git rebase --keep-base --signoff upstream/main` then
   `git push --force-with-lease` (use the PR's base, up to date; a stale local
   `main` replays other people's commits onto the branch).
4. Every commit also carries a verified signature: `main` requires one, and a
   branch without it cannot merge. Sign with a GPG or SSH key registered on
   your GitHub account, under an email address the account has verified
   (`git config commit.gpgsign true`). Unsigned commits on an existing branch:
   `git rebase --keep-base --force-rebase --gpg-sign upstream/main` then
   `git push --force-with-lease`. `--force-rebase` matters: without it, git sees
   the branch already sits on its base, rewrites nothing, and every commit
   stays unsigned.

## Opening the PR

5. **The PR title is the commit.** This repository squash-merges, so the title
   becomes the one commit subject on `main` and is what the `pr-title` check
   and the branch rule validate. It must be a Conventional Commit
   (`type(scope): description`, lower case). Branch commits are discarded by
   the squash; do not polish them.
6. Reference the issue with `Closes #<N>` for non-trivial changes (the repo is
   issue-first — open one before a significant PR).
7. Apply exactly one change-category label: `feature`, `bug`, or
   `documentation`. There is no `breaking` label; mark a breaking change
   with `!` in the title or a `BREAKING CHANGE:` footer.
8. If a substantial portion of the change was AI-generated, disclose that in
   the PR description (required by CONTRIBUTING.md). Do not add agent
   attribution: no `Co-Authored-By` trailer naming an agent in any commit, and
   no "Generated with …" footer in the PR description.

## After opening — verify checks are PRESENT, not just green

The advisory checks, most of them on self-hosted runners, build through a
`pull-request/<number>` branch created by copy-pr-bot, which gates on **author vetting**. For unvetted
authors those checks are never created, and **their absence looks identical to
success**: a column of green ticks with checks simply missing.

```bash
gh pr checks <number> --repo dsx-ai-factory/nvke --json name,workflow
```

Which checks block a merge is a repository setting. Read it with
`gh api repos/dsx-ai-factory/nvke/rules/branches/main --jq '[.[].type]'`. The
required checks, `gate` and `DCO`, always report; golangci-lint, govulncheck,
actionlint, shellcheck and the notices gate run inside `gate`. The checks
that depend on the mirror, among them CodeQL, gitleaks, grype, the image
build, the OS-matrix unit tests, the package reproducibility build, the OS
image builds and the end-to-end suites, are legitimately absent for unvetted
authors. Flag that state rather than treating it as passing.
