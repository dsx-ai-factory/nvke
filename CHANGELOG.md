# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**This file is generated — do not edit it by hand.** See `cliff.toml`.

## [Unreleased]

### Added

- Import cloud-workflow-engine

### Fixed

- *(auth)* Use constant-time comparison and correct gRPC status codes
- *(lint)* Enable linters that were in both enable and disable lists
- Address review feedback — rename node-agent to nke-agent in docs
- Center agent/ label in architecture diagram
- Correct diagram right-box width (off-by-one trailing space)
- Correct diagram outer box width on lines 21-22
- Use recursive glob so nested controller modules are covered
- *(notices)* Pin collation so the generated file is the same everywhere
- *(ci)* Make the SARIF uploads actually run
- *(ci)* De-duplicate govulncheck SARIF tags before upload
- Run the license header check instead of skipping it

### Documentation

- *(node-agent)* Replace SWE/CWE references with NKE controller
- Add root-level docs/ directory with architecture and getting-started guides
- Add root-level AGENTS.md for AI coding agent guidance
- Add GOVERNANCE.md and MAINTAINERS.md
- Adopt recommended public-facing SECURITY.md
- Add SUPPORT.md
- Fill in development setup in CONTRIBUTING.md
- Replace the template README with a real one
- Add THIRD_PARTY_NOTICES.md for the agent, with a generator and CI check
- Route vulnerability reports to PSIRT and complete the security policy
- Write a real root AGENTS.md
- Add the root NOTICE and a THIRD_PARTY_NOTICES index
- Correct the DCO sign-off instructions
- Fix paths and check names that no longer match the repository
- Add a documentation issue template
- Adopt the Contributor Covenant 2.1 Code of Conduct
- Generate CHANGELOG.md with git-cliff instead of by hand

### Maintenance

- NKX-14119 add Apache-2.0 LICENSE
- Add OSS community health files (code of conduct, contributing, PR/issue templates)
- Enable copy-pr-bot
- Add golangci-lint workflow for node-agent
- Add CHANGELOG.md, RELEASE.md, and automated release workflow
- Add CODEOWNERS and branch protection settings guide
- Add dependabot config for go monorepo
- Raise PR limit, group all ecosystems, conventional-commit prefix
- Bump the github-actions group with 2 updates
- Fix CODEOWNERS to use @NVIDIA/nke-maintainer team
- Split the Kubernetes stack into its own Dependabot group
- Parameterize the container base image registry
- Use the runner BuildKit config for the buildx builder
- Add an image build check
- Add DCO configuration and document sign-off
- Fix the stale CODEOWNERS path and the phantom maintainer team
- NKX-14119 add GitHub Actions license enforcement for cloud-workflow-engine
- NKX-14119 use dynamic module discovery for license checks
- NKX-14119 generalize release gate for all monorepo components
- NKX-14119 ignore internal packages in go-licenses check
- NKX-14119 fix go-licenses failures on stdlib and private deps
- Add Apache-2.0 license headers and check to agent
- Add govulncheck, CodeQL and Grype scans
- Remove calico sample manifest from kubeadm-deploy
- Remove legacy bundle-based CNI install path
- Scan every pull request for committed secrets

### Other

- Initial commit
- Add initial files from template
- Add initial files from template
- Migrate node-agent into the monorepo with GitHub module paths.
- Slim the node-agent README and use clearly fake example secrets.
- Remove full-disk encryption / TPM unlock from the node-agent.
- Add component versioning and consumer-configurable artifact endpoints.
- Rename node-agent to agent.
- Point controller at the in-tree agent module.
- Update CHANGELOG.md


