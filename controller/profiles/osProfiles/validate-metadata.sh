#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary

set -euo pipefail

# Validate that all OSProfile directories have metadata entries

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
METADATA_FILE="${SCRIPT_DIR}/metadata.yaml"
DEPRECATED_DIR="${SCRIPT_DIR}/deprecated"
DEPRECATED_FILE="${SCRIPT_DIR}/deprecated.yaml"

if [[ ! -f "$METADATA_FILE" ]]; then
  echo "ERROR: metadata.yaml not found at $METADATA_FILE"
  exit 1
fi

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Validating OSProfile metadata..."
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

VALIDATION_FAILED=0
MISSING_DEPRECATED_METADATA=()
ORPHANED_DEPRECATED_METADATA=()

# Step 1: Check that all profile directories have metadata entries
echo ""
echo "Step 1: Checking for profile directories without metadata..."
echo "------------------------------------------------------------"

MISSING_METADATA=()
for profile_dir in "$SCRIPT_DIR"/nke-*; do
  if [[ ! -d "$profile_dir" ]]; then
    continue
  fi
  
  profile_name=$(basename "$profile_dir")
  
  # Check if this profile exists in metadata.yaml
  if ! yq eval ".profiles | has(\"$profile_name\")" "$METADATA_FILE" | grep -q "true"; then
    MISSING_METADATA+=("$profile_name")
    echo "  [ERROR] MISSING: $profile_name (directory exists but no metadata entry)"
    VALIDATION_FAILED=1
  fi
done

if [[ ${#MISSING_METADATA[@]} -eq 0 ]]; then
  echo "  [OK] All profile directories have metadata entries"
fi

# Step 2: Check that metadata entries correspond to actual directories (orphans are errors)
echo ""
echo "Step 2: Checking for orphaned metadata entries..."
echo "------------------------------------------------------------"

ORPHANED_METADATA=()
PROFILES=$(yq eval '.profiles | keys | .[]' "$METADATA_FILE")

while IFS= read -r profile_name; do
  [[ -z "$profile_name" ]] && continue

  profile_dir="$SCRIPT_DIR/$profile_name"
  if [[ ! -d "$profile_dir" ]]; then
    ORPHANED_METADATA+=("$profile_name")
    echo "  [ERROR] ORPHANED: $profile_name (metadata.yaml entry exists but no current profile directory. If the profile has been deprecated, the entry must be moved to deprecated.yaml)"
    VALIDATION_FAILED=1
  fi
done <<< "$PROFILES"

if [[ ${#ORPHANED_METADATA[@]} -eq 0 ]]; then
  echo "  [OK] All metadata.yaml entries have corresponding current profile directories"
fi

# Step 3: If deprecated/ exists, every profile in it must have an entry in deprecated.yaml
if [[ -d "$DEPRECATED_DIR" ]]; then
  echo ""
  echo "Step 3: Checking for deprecated profile directories without metadata..."
  echo "------------------------------------------------------------"

  if [[ ! -f "$DEPRECATED_FILE" ]]; then
    echo "  [ERROR] deprecated/ exists but deprecated.yaml not found at $DEPRECATED_FILE"
    VALIDATION_FAILED=1
  else
    MISSING_DEPRECATED_METADATA=()
    for profile_dir in "$DEPRECATED_DIR"/nke-*; do
      if [[ ! -d "$profile_dir" ]]; then
        continue
      fi

      profile_name=$(basename "$profile_dir")

      if ! yq eval ".profiles | has(\"$profile_name\")" "$DEPRECATED_FILE" | grep -q "true"; then
        MISSING_DEPRECATED_METADATA+=("$profile_name")
        echo "  [ERROR] MISSING: $profile_name (directory in deprecated/ but no entry in deprecated.yaml)"
        VALIDATION_FAILED=1
      fi
    done

    if [[ ${#MISSING_DEPRECATED_METADATA[@]} -eq 0 ]]; then
      echo "  [OK] All deprecated profile directories have entries in deprecated.yaml"
    fi
  fi
fi

# Step 4: If deprecated/ and deprecated.yaml exist, every entry in deprecated.yaml must have a directory in deprecated/
if [[ -d "$DEPRECATED_DIR" ]] && [[ -f "$DEPRECATED_FILE" ]]; then
  echo ""
  echo "Step 4: Checking for orphaned deprecated metadata entries..."
  echo "------------------------------------------------------------"

  ORPHANED_DEPRECATED_METADATA=()
  DEPRECATED_PROFILES=$(yq eval '.profiles | keys | .[]' "$DEPRECATED_FILE")
  while IFS= read -r profile_name; do
    [[ -z "$profile_name" ]] && continue

    profile_dir="$DEPRECATED_DIR/$profile_name"
    if [[ ! -d "$profile_dir" ]]; then
      ORPHANED_DEPRECATED_METADATA+=("$profile_name")
      echo "  [ERROR] ORPHANED: $profile_name (entry in deprecated.yaml but no directory in deprecated/)"
      VALIDATION_FAILED=1
    fi
  done <<< "$DEPRECATED_PROFILES"

  if [[ ${#ORPHANED_DEPRECATED_METADATA[@]} -eq 0 ]]; then
    echo "  [OK] All deprecated.yaml entries have corresponding directories in deprecated/"
  fi
fi

# Step 5: Find https/s3/cloudfront URLs and validate with HEAD (must be 2xx)
# When VALIDATE_METADATA_CHANGED_FILES is set (e.g. in CI MR), only validate URLs in changed files.
# URLs matching patterns in url-validation-skip-list.txt are allowed to return non-2xx (legacy endpoints).
echo ""
echo "Step 5: Validating URLs (HEAD request, expect 2xx)..."
echo "------------------------------------------------------------"

if ! command -v curl &>/dev/null; then
  echo "  [ERROR] curl is required for URL validation but is not installed"
  VALIDATION_FAILED=1
else
SKIP_LIST_FILE="${SCRIPT_DIR}/url-validation-skip-list.txt"
# Regex for extracting https URLs (used in grep -E); double-quoted so single quote is literal
URL_EXTRACT_REGEX="https://[^][[:space:]\"'<>{}|]+"

# Extract https URLs with source path (relpath|url) for profile display.
# Never scan deprecated/ or deprecated.yaml - those may reference stale URLs not required to be reachable.
# If VALIDATE_METADATA_CHANGED_FILES points to a file listing paths (relative to SCRIPT_DIR), only scan those files.
extract_https_urls_with_source() {
  local relpath f
  if [[ -n "${VALIDATE_METADATA_CHANGED_FILES:-}" ]] && [[ -f "$VALIDATE_METADATA_CHANGED_FILES" ]]; then
    local count
    count=$(wc -l < "$VALIDATE_METADATA_CHANGED_FILES" 2>/dev/null || echo 0)
    if [[ "$count" -gt 0 ]]; then
      while IFS= read -r relpath; do
        [[ -z "$relpath" ]] && continue
        [[ "$relpath" == deprecated.yaml ]] && continue
        [[ "$relpath" == deprecated/* ]] && continue
        # Only scan URL-bearing file types (same as full-scan mode)
        case "$relpath" in
          *.yaml|*.yml|*.tmpl) : ;;
          *) [[ "$(basename "$relpath")" == "ipxeScript" ]] || continue ;;
        esac
        f="${SCRIPT_DIR}/${relpath}"
        if [[ -f "$f" ]]; then
          grep -hoE "$URL_EXTRACT_REGEX" "$f" 2>/dev/null | while IFS= read -r url; do echo "${relpath}|${url}"; done
        fi
      done < "$VALIDATE_METADATA_CHANGED_FILES"
      return
    fi
  fi
  while IFS= read -r f; do
    [[ -f "$f" ]] || continue
    relpath="${f#${SCRIPT_DIR}/}"
    grep -hoE "$URL_EXTRACT_REGEX" "$f" 2>/dev/null | while IFS= read -r url; do echo "${relpath}|${url}"; done
  done < <(find "$SCRIPT_DIR" -type f \( -name '*.yaml' -o -name '*.tmpl' -o -name 'ipxeScript' -o -name '*.yml' \) ! -path "${SCRIPT_DIR}/deprecated/*" ! -name "deprecated.yaml" 2>/dev/null)
}

# Derive OS profile name from source path for display
profile_from_source() {
  local src="$1"
  if [[ "$src" == deprecated/* ]]; then
    echo "$src" | cut -d/ -f1-2
    return
  fi
  if [[ "$src" == nke-* ]]; then
    echo "${src%%/*}"
    return
  fi
  basename "$src"
}

# Load legacy URL skip list (bash ERE patterns; one per line; # and empty lines ignored)
LEGACY_SKIP_PATTERNS=()
if [[ -f "$SKIP_LIST_FILE" ]]; then
  while IFS= read -r line; do
    line="${line%%#*}"
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -n "$line" ]] && LEGACY_SKIP_PATTERNS+=("$line")
  done < "$SKIP_LIST_FILE"
fi

url_matches_legacy_skip_list() {
  local url="$1"
  local pat
  for pat in "${LEGACY_SKIP_PATTERNS[@]}"; do
    [[ "$url" =~ $pat ]] && return 0
  done
  return 1
}

URL_VALIDATION_FAILED=0
CHECKED_URLS=()
FAILED_URLS=()

# Dedupe by URL (keep first source for profile), then validate each unique URL
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  source="${line%%|*}"
  url="${line#*|}"
  [[ -z "$url" ]] && continue
  # Skip URLs with template syntax or placeholder (cannot be resolved for HEAD)
  # Use double quotes for "{" to avoid bash parse confusion with *'{'*
  if [[ "$url" == *'{{'* ]] || [[ "$url" == *'${'* ]] || [[ "$url" == *'<'* ]] || [[ "$url" == *'>'* ]] || [[ "$url" == *"{"* ]]; then
    continue
  fi
  if [[ "$url" =~ _v$ ]] || [[ "$url" =~ /v$ ]]; then
    continue
  fi
  if [[ "$url" =~ ^https://[^/]+/?$ ]]; then
    continue
  fi
  if [[ ${#CHECKED_URLS[@]} -gt 0 ]] && [[ " ${CHECKED_URLS[*]} " == *" $url "* ]]; then
    continue
  fi
  # Skip base/prefix URLs (last segment doesn't end with a file extension like .iso, .tgz, .efi)
  if [[ ! "${url##*/}" =~ \.[a-zA-Z]{2,}[0-9]*$ ]]; then
    continue
  fi
  profile=$(profile_from_source "$source")
  CHECKED_URLS+=("$url")
  code=""
  code=$(curl -sI -o /dev/null -w "%{http_code}" --max-time 10 --retry 1 -L "$url" 2>/dev/null || echo "000")
  if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then
    echo "  [OK] $url ($code) $profile"
  else
    if url_matches_legacy_skip_list "$url"; then
      echo "  [SKIP] $url (legacy, HTTP $code) $profile"
    else
      echo "  [ERROR] $url (HTTP $code) $profile"
      FAILED_URLS+=("$url")
      URL_VALIDATION_FAILED=1
    fi
  fi
done < <(extract_https_urls_with_source | sort -t'|' -k2,2 -u)

if [[ ${#CHECKED_URLS[@]} -eq 0 ]]; then
  echo "  [OK] No https URLs found to validate"
elif [[ $URL_VALIDATION_FAILED -eq 0 ]]; then
  echo "  [OK] All ${#CHECKED_URLS[@]} URL(s) returned 2xx or were skipped (legacy)"
fi

[[ $URL_VALIDATION_FAILED -ne 0 ]] && VALIDATION_FAILED=1
fi

# Summary
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Validation Summary"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Profiles with metadata: $(yq eval '.profiles | length' "$METADATA_FILE")"
echo "  Profile directories: $(find "$SCRIPT_DIR" -maxdepth 1 -type d -name 'nke-*' | wc -l | tr -d ' ')"

if [[ -d "$DEPRECATED_DIR" ]] && [[ -f "$DEPRECATED_FILE" ]]; then
  echo "  Deprecated profiles (deprecated.yaml): $(yq eval '.profiles | length' "$DEPRECATED_FILE")"
  echo "  Deprecated profile directories: $(find "$DEPRECATED_DIR" -maxdepth 1 -type d -name 'nke-*' | wc -l | tr -d ' ')"
fi

if [[ ${#MISSING_METADATA[@]} -gt 0 ]]; then
  echo "  Missing metadata: ${#MISSING_METADATA[@]}"
fi

if [[ ${#ORPHANED_METADATA[@]} -gt 0 ]]; then
  echo "  Orphaned metadata: ${#ORPHANED_METADATA[@]}"
fi

if [[ -d "$DEPRECATED_DIR" ]] && [[ -f "$DEPRECATED_FILE" ]] && [[ ${#MISSING_DEPRECATED_METADATA[@]} -gt 0 ]]; then
  echo "  Deprecated dirs missing from deprecated.yaml: ${#MISSING_DEPRECATED_METADATA[@]}"
fi

if [[ -d "$DEPRECATED_DIR" ]] && [[ -f "$DEPRECATED_FILE" ]] && [[ ${#ORPHANED_DEPRECATED_METADATA[@]} -gt 0 ]]; then
  echo "  Orphaned deprecated.yaml entries: ${#ORPHANED_DEPRECATED_METADATA[@]}"
fi

if [[ ${#FAILED_URLS[@]} -gt 0 ]]; then
  echo "  URLs failing HEAD (non-2xx): ${#FAILED_URLS[@]}"
fi

echo ""
if [[ $VALIDATION_FAILED -eq 0 ]]; then
  echo "[PASS] Validation PASSED"
  exit 0
else
  echo "[FAIL] Validation FAILED"
  echo ""
  echo "Please ensure:"
  echo "  - All profile directories have metadata.yaml entries in metadata.yaml"
  echo "  - No orphaned entries in metadata.yaml (every metadata.yaml entry must have a matching directory)"
  if [[ -d "$DEPRECATED_DIR" ]]; then
    echo "  - Every profile in deprecated/ has an entry in deprecated.yaml"
    echo "  - Every entry in deprecated.yaml has a directory in deprecated/"
  fi
  if [[ ${#FAILED_URLS[@]} -gt 0 ]]; then
    echo "  - All https/s3/cloudfront URLs in osProfiles return 2xx on HEAD (fix or remove broken URLs)"
  fi
  exit 1
fi
