# Governance

## Overview

NVKE is an open-source project maintained by NVIDIA. This document
describes how the project is governed, how decisions are made, and how
to become a maintainer.

## Roles

### Contributors

Anyone who submits a pull request or files an issue. No formal
nomination required. Contributors are expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md) and [CONTRIBUTING.md](CONTRIBUTING.md).

### Maintainers

Maintainers have write access to the repository and are responsible for:

- Reviewing and merging pull requests
- Triaging issues
- Cutting releases (see [RELEASE.md](RELEASE.md))
- Upholding the code of conduct

Current maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md).

## Decision-making

### Routine changes

Bug fixes, documentation updates, and small improvements are merged by
any single maintainer after at least one approving review (including
from a non-author maintainer for larger changes).

### Significant changes

Significant changes — new features, breaking API changes, architectural
decisions, or changes to governance itself — require:

1. An issue or discussion opened at least **5 business days** before a
   PR is submitted, to allow async feedback.
2. Consensus among active maintainers (no unresolved objections from
   maintainers within the discussion window).
3. At least **2 approving reviews** before merge.

If consensus cannot be reached, the project lead (see MAINTAINERS.md)
makes the final call.

### Security issues

Security issues follow the process in [SECURITY.md](SECURITY.md) and
bypass the public discussion requirement.

## Becoming a maintainer

Contributors who have made sustained, high-quality contributions may be
nominated for maintainership by an existing maintainer. Nomination is
approved by consensus of the current maintainers. Maintainers who are
inactive for 6+ months may be moved to emeritus status.

## Project lead

The project lead is an NVIDIA employee responsible for strategic
direction and final tie-breaking decisions. See MAINTAINERS.md for the
current project lead.

## Changes to governance

Proposed changes to this document follow the "significant changes"
process above, with a minimum 10-business-day discussion window.
