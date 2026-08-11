# Changelog

All notable changes to nke are documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Code of Conduct upgraded from Contributor Covenant 1.4 to 2.1, adding the four-step Community Impact enforcement ladder

### Added
- Root-level `docs/` directory with architecture overview and getting-started guide
- `.github/CODEOWNERS` for automatic review assignment
- `.github/workflows/lint.yml` — golangci-lint for the agent module
- `.github/workflows/notices-check.yml` — fails when `THIRD_PARTY_NOTICES.md` is stale
- `.github/workflows/image.yml` — multi-architecture container image build
- `.github/workflows/release.yml` — creates the GitHub release for a tag
- `SUPPORT.md` — contributor support channels
- `GOVERNANCE.md` — project governance and decision-making
- `MAINTAINERS.md` — current maintainers list
- `RELEASE.md` — release process documentation
- `AGENTS.md` — AI agent guide for the monorepo
- `.github/ISSUE_TEMPLATE/documentation_request.yml` — issue form for documentation requests
- `.github/workflows/govulncheck.yml` — reports Go vulnerabilities the agent module calls
- `.github/workflows/codeql.yml` — CodeQL static analysis of the agent module
- `.github/workflows/security-scan.yml` — Anchore/Grype dependency scan of the tree
- `.github/workflows/secrets.yml` — gitleaks scan of the commits a pull request adds
- `.github/actionlint.yaml` — self-hosted runner labels, so actionlint runs clean

### Changed
- `CONTRIBUTING.md` — replaced TODO placeholders with actual build/test commands
- `SECURITY.md` — added GitHub private vulnerability reporting path

### Fixed
- Documentation paths left stale by the `node-agent/` → `agent/` rename, in
  `docs/getting-started.md`, `docs/README.md`, `SUPPORT.md` and
  `.github/dependabot.yml`
- `docs/getting-started.md` — dropped the reference to `node-agent/integration-test/`,
  a directory that does not exist in this repository
- `docs/getting-started.md` — recorded that `controller/` cannot build outside the
  NVIDIA network, and that the agent tests assume Linux
- `docs/branch-protection-settings.md` — listed status checks from workflows that
  were never added; replaced with the checks the current workflows report
- `docs/README.md` and `docs/architecture.md` — both pointed at a
  `controller/docs/DESIGN.md` that has never existed

---

*Older history is tracked in git. Run `git log --oneline` for a full list of commits.*
