#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Check that the working tree names no internal infrastructure.
#
# WHY THIS EXISTS, AND WHY IT IS NOT gitleaks. gitleaks answers "was a
# credential ever committed", over a range of history. This answers a different
# question -- "does the tree that ships describe infrastructure only NVIDIA can
# see" -- and that has to be about the tree, because the tree is what becomes
# public. None of what this looks for is a credential: a site id grants
# nothing, a jump host's name grants nothing. They are a map, and a map is the
# thing a reader outside does not otherwise have.
#
# BY DIGEST, NOT BY PATTERN. A check that listed the identifiers it forbids
# would publish them itself, which is the whole defect it exists to prevent. So
# it extracts every identifier-shaped token in the tree, hashes each one, and
# compares against the digests below. Nothing internal is written down here.
#
# The limit of that: a UUID has enough entropy that its SHA-256 is not
# reversible, which is why the list is UUIDs. Shorter values are not protected
# by a digest and are handled by shape instead -- see SHAPES, which names
# nothing internal either.
#
# NARROW ON PURPOSE. A check that goes red for reasons nobody can act on is
# ignored within a week, and then it protects nothing. It does not flag RFC1918
# addresses, because pod and service CIDRs are legitimately 10.x throughout. It
# does not flag bare UUIDs by shape, because there are thousands and nearly all
# are fixtures. It does not flag *.nvidia.com, because nvcr.io, ngc.nvidia.com
# and authn.nvidia.com are how a public consumer reaches NVIDIA.
#
# Adding an identifier: print its SHA-256 and paste the digest, never the
# value.
#   printf '%s' "<the identifier>" | shasum -a 256
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

# SHA-256 of each internal identifier, with what it is. The label is what the
# failure prints, so it says enough to act on and no more.
declare -a DIGESTS=(
  # The RelEng Forge account, moved to FORGE_* repository variables.
  "583affae05d80d468475489c07514c5de86860f1b6e5f01eb0556424616a4b42|Forge site id (use FORGE_SITE_ID)"
  "184d325c7293135efb2952d918268cfbebc2779ef0137727b53ba70fd0b78665|Forge VPC id (use FORGE_VPC_ID)"
  "2994877a0a23c05ad7ee41dc96cf4899a8f47c36ced4ebf6b2abf496525dd341|Forge subnet id (use FORGE_SUBNET_ID)"
  "8dc7ea02b77b218d40fd7417679f94e2fe28d49ca8ccd424d0f0b793451e1aaf|Forge SSH key group id (use FORGE_SSH_KEY_GROUP_ID)"
  "2b74d26b11280d0ad6111376933feeeab067137be40ecc7f0823ceb3012a21b4|Forge CPU instance type id (use FORGE_INSTANCE_TYPE_CPU_ID)"
  "460d26f4902410c07bd923278947002daf2a3928d1efcadf23f1f0b251f712b7|Forge GPU instance type id (use FORGE_INSTANCE_TYPE_GPU_ID)"
  # The site that arrived with the cloud-workflow-engine import and was never
  # used, plus the instance types listed beside it. Removed rather than moved:
  # nothing needs them.
  "d43ec83a9fccfc5f2644f0141700cc6a58769d4e25f56968d803d485fffa259a|retired Forge site id, remove it"
  "a1a473925115e9d8c70c45a0dc7fb24428162569b2c5248a434fc2ae1afcdc17|retired Forge VPC id, remove it"
  "7539e3924f9ee30827be1292384fdcf283af52b696c3978544bf2a81da7b17f9|retired Forge subnet id, remove it"
  "173c6a75598d7f180b18b6d0bd79c94a437831525c76e39317980769e5d471b3|retired Forge SSH key group id, remove it"
  "04854307ef16a925a3a2c1ae4d2a1f7ac9c6d0e6fe8c08c36266205963f36146|retired Forge instance type id, remove it"
  "2786723ef31263f471f5a8879708d13a46de25d1b9d43fd2c2064fe5e9e8c0db|retired Forge instance type id, remove it"
  "13161b5820d08b396d1507e0db9869007249d3a6c7b27fd153459b0f6650ff67|retired Forge instance type id, remove it"
  "df057cfc532b652d97b8e2aa86e5b9039117e89c7f41aad1cf999d741539ce29|retired Forge instance type id, remove it"
  "f5e9d00897d0917cb667b24c0ac9be9e14dd610bcb1a32f91bc29e0c88bc55d0|retired Forge instance type id, remove it"
)

# Shapes that need no internal value to describe. Each is a PCRE2 pattern and a
# label.
declare -a SHAPES=(
  # An NGC or Forge org id: sixteen digits, which nothing else here is shaped
  # like. A repeated single digit is a placeholder and passes.
  '(?<![0-9])(?!([0-9])\1{15})[0-9]{16}(?![0-9])|sixteen-digit NGC or Forge org id'
  # The internal switch and node naming scheme embeds a management address in
  # the hostname: 10-91-69-85.<site>.<domain>.
  '\b10-[0-9]{1,3}-[0-9]{1,3}-[0-9]{1,3}\.[a-z]|management address embedded in a hostname'
)

# vendor/ is third-party source, verbatim. The sum and lock files are
# content-addressed upstream artifacts. This file is excluded from the shape
# pass because it describes the shapes it looks for.
readonly -a EXCLUDES=(
  --glob '!vendor/**'
  --glob '!.git/**'
  --glob '!**/*.sum'
  --glob '!**/*.lock'
  --glob '!hack/internal-config-check.sh'
  # An NGC org appears in the chart repository URL this installs from, which a
  # consumer of the dev charts needs in order to resolve them. Tracked to move
  # to a variable with the rest; exempt by path so the check is actionable
  # today rather than red for something nobody is about to change.
  --glob '!controller/hack/nico-provider/install-capnico.sh'
)

FAILED=0

printf 'internal-config-check: %s\n\n' "${ROOT}"

# --- identifiers, by digest ------------------------------------------------

# Every UUID in the tree, as path:line:value. Hashing happens per distinct
# value rather than per occurrence, so a fixture repeated a hundred times costs
# one hash.
candidates="$(rg --hidden --no-heading --line-number --only-matching \
  "${EXCLUDES[@]}" --glob '!hack/internal-config-check.sh' \
  -- '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' . 2>/dev/null || true)"

matched=""
while IFS= read -r value; do
  [[ -n "${value}" ]] || continue
  digest="$(printf '%s' "${value}" | shasum -a 256 | awk '{print $1}')"
  for entry in "${DIGESTS[@]}"; do
    if [[ "${digest}" == "${entry%%|*}" ]]; then
      label="${entry##*|}"
      printf '  FAIL  %s\n' "${label}"
      # The value is already in the tree, but printing it puts it in a CI log
      # that is readable by more people than the tree is. Locations only.
      printf '%s\n' "${candidates}" | rg -F -- ":${value}" | sed 's/:[0-9a-f-]*$//' | sed 's/^/        /'
      matched="yes"
      FAILED=1
    fi
  done
done < <(printf '%s\n' "${candidates}" | sed 's/.*://' | sort -u)

[[ -n "${matched}" ]] || printf '  ok    no known internal identifier\n'

# --- shapes ----------------------------------------------------------------

for entry in "${SHAPES[@]}"; do
  pattern="${entry%|*}"
  label="${entry##*|}"

  if hits="$(rg --pcre2 --hidden --no-heading --line-number "${EXCLUDES[@]}" -- "${pattern}" . 2>/dev/null)"; then
    printf '  FAIL  %s\n' "${label}"
    printf '%s\n' "${hits}" | sed 's/^/        /'
    FAILED=1
  else
    printf '  ok    no %s\n' "${label}"
  fi
done

printf '\n'
if [[ "${FAILED}" -ne 0 ]]; then
  cat >&2 <<'GUIDANCE'
internal-config-check: the tree names internal infrastructure.

None of this is a credential, so there is nothing to rotate. Move the value to
a repository variable and read it at run time -- see
docs/qualification/environments.md, "Where the identifiers come from". A
comment that merely mentions one should lose it: say what role the thing plays
rather than which one it is.

If a match is a fixture rather than a real identifier, make it obviously
synthetic -- an all-zero UUID, an address in 192.0.2.0/24 -- rather than
exempting it.
GUIDANCE
  exit 1
fi

printf 'internal-config-check: clean\n'
