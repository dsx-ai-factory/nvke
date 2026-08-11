# Release process

This document describes how nke releases are created and published.

## Versioning

nke uses [Semantic Versioning](https://semver.org/):

```
v<MAJOR>.<MINOR>.<PATCH>
```

| Increment | When |
|---|---|
| `PATCH` | Backwards-compatible bug fixes |
| `MINOR` | New backwards-compatible features |
| `MAJOR` | Breaking API or behavioral changes |

## Release cadence

There is no fixed release schedule. Releases are made when a meaningful
set of features or fixes has accumulated on `main`, or when a security
patch requires an immediate out-of-band release.

## Creating a release

### 1. Update CHANGELOG.md

`CHANGELOG.md` is generated from commit subjects by
[git-cliff](https://git-cliff.org), configured in `cliff.toml`. **Do not edit it
by hand.**

```bash
# 1. Drop the pending [Unreleased] section. Without this, --prepend leaves it
#    in place holding the same entries the new section just claimed, so the
#    file lists everything twice and keeps a stale [Unreleased] forever.
awk '/^## \[Unreleased\]/{s=1} /^## \[[0-9]/{s=0} !s' CHANGELOG.md > /tmp/cl && mv /tmp/cl CHANGELOG.md

# 2. Prepend the new section. --prepend leaves published sections byte-identical;
#    regenerating the whole file re-derives their dates and rewrites history.
git-cliff --unreleased --tag v0.2.0 --prepend CHANGELOG.md
```

Run this in a **full clone**. On a shallow one git-cliff emits a near-empty
section and exits 0, so nothing fails.

```bash
git commit -s -m "chore: update CHANGELOG.md for v0.2.0"
```

**The heading carries no `v` prefix.** Step 3 and
`.github/workflows/release.yml` both read the section back by that heading, and
neither fails when it does not match — they emit a fallback line — so a mismatch
ships a release with no notes rather than an error.

### 2. Tag the release

```bash
git tag -s v0.2.0 -m "Release v0.2.0"
git push upstream v0.2.0
```

### 3. Create a GitHub release

```bash
gh release create v0.2.0 \
  --title "v0.2.0" \
  --notes-file <(sed -n '/^## \[0.2.0\]/,/^## \[/{ /^## \[0.2.0\]/d; /^## \[/d; p }' CHANGELOG.md)
```

Or use the GitHub UI: **Releases → Draft a new release → Choose tag → Generate release notes**.

### 4. Verify artifacts

After the tag push, confirm that any CI release pipelines complete
successfully (image build, chart publish, etc.).

## Hotfix releases

For security patches that cannot wait for the next regular release:

1. Branch from the tag that needs patching: `git checkout -b fix/v0.1.1 v0.1.0`
2. Cherry-pick or apply the fix.
3. Follow steps 1–4 above with the patch version incremented.
4. Backport to `main` if the fix applies there too.

## Pre-releases

For release candidates, append `-rc.N` to the version: `v0.2.0-rc.1`.
Tag and publish the same way; GitHub marks pre-release tags automatically.
