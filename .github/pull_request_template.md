## Summary

<!-- What changed? Keep it short (1-2 sentences). -->
<!-- Note: CHANGELOG.md is generated from commit subjects by git-cliff.
     This repository squash-merges, so the pull request TITLE becomes the
     single commit subject on main and therefore the changelog entry.
     Write it for a reader. -->

## Motivation / Context

<!-- Why is this change needed? Link issues/discussions. -->

Closes: <!-- #123 or N/A -->
Related: <!-- #123 or N/A -->

## Type of Change

<!-- Check all that apply -->

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to change)
- [ ] Documentation update
- [ ] Refactoring (no functional changes)
- [ ] Build/CI/tooling

## Component(s) Affected

- [ ] Controller (`controller/`)
- [ ] REST API (`controller/api/openapi.yaml`)
- [ ] Node agent or updater (`agent/`)
- [ ] gRPC API (`agent/pkg/agent/proto/`)
- [ ] nvkectl CLI (`cli/`)
- [ ] Cluster API provider (`cluster-api-provider-provisioned/`)
- [ ] Helm charts or deployment (`controller/deploy/`)
- [ ] NKTP telemetry pipeline (`nktp/`)
- [ ] nico-sim (`controller/nico-sim/`)
- [ ] OS image or image checker (`os-image/`)
- [ ] CI / workflows (`.github/`)
- [ ] Docs / examples
- [ ] Other: ____________

## Implementation Notes

<!-- Key decisions, trade-offs, and any non-obvious behavior changes. Delete if not applicable. -->

## Testing

```bash
# Commands run. `make preflight` lints and tests the modules you changed;
# see docs/development.md for what it does not cover.
make preflight
```

<!-- Summarize test results or paste relevant output.
     If you changed controller/api/openapi.yaml, also run `make api-compat-check` in controller/. -->

## Risk Assessment

<!-- Select one and explain -->

- [ ] **Low** — Isolated change, well-tested, easy to revert
- [ ] **Medium** — Touches multiple components or has broader impact
- [ ] **High** — Breaking change, affects critical paths, or complex rollout

**Rollout notes:** <!-- Migration steps, feature flags, backwards compatibility, or N/A -->

## Checklist

- [ ] I am familiar with the [Contributing Guidelines](https://github.com/dsx-ai-factory/nvke/blob/main/CONTRIBUTING.md)
- [ ] Every commit carries a DCO sign-off (`git commit -s`)
- [ ] `make preflight` passes locally
- [ ] I did not skip/disable tests to make CI green
- [ ] I added/updated tests for new functionality
- [ ] I updated docs if user-facing behavior changed
- [ ] I regenerated generated output rather than editing it by hand (if applicable)
- [ ] I disclosed above if a substantial portion of this change was AI-generated
