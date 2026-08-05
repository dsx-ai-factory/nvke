#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Updates an FDE qcow OS profile with a new image URL:
#   1. Creates a new profile by copying the current one
#   2. Updates the qcow2 image URL in the ipxeScript
#   3. Deprecates the old profile (moves dir + metadata)
#
# Usage:
#   ./update-qcow-profile.sh <new-qcow2-url>
#
# Example:
#   ./update-qcow-profile.sh https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/x86_64/lvm/2026-03-23/1774290565/nke-24.04-x86_64-lvm-2026-03-23-1774290565.qcow2

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
METADATA_FILE="${SCRIPT_DIR}/metadata.yaml"
DEPRECATED_FILE="${SCRIPT_DIR}/deprecated.yaml"
DEPRECATED_DIR="${SCRIPT_DIR}/deprecated"

# --- Argument parsing ---
if [[ $# -ne 1 ]]; then
  echo "Usage: $0 <new-qcow2-url>"
  echo "Example: $0 https://d3mq564cn7sth7.cloudfront.net/nke-isos/qcow/images/24.04/x86_64/lvm/2026-03-23/1774290565/nke-24.04-x86_64-lvm-2026-03-23-1774290565.qcow2"
  exit 1
fi

NEW_IMAGE_URL="$1"

# --- Validate URL format ---
if [[ ! "$NEW_IMAGE_URL" =~ \.qcow2$ ]]; then
  echo "ERROR: URL must end with .qcow2"
  exit 1
fi

# --- Extract metadata from URL ---
# URL pattern: .../images/<ubuntu_ver>/<cpu_arch>/lvm/<date>/<build_id>/<filename>.qcow2
# Example: .../images/24.04/x86_64/lvm/2026-03-23/1774290565/nke-24.04-x86_64-lvm-2026-03-23-1774290565.qcow2

# Extract architecture from URL path (x86_64 or aarch64)
if [[ "$NEW_IMAGE_URL" =~ /images/[^/]+/([^/]+)/ ]]; then
  URL_ARCH="${BASH_REMATCH[1]}"
else
  echo "ERROR: Cannot detect architecture from URL. Expected pattern: /images/<ubuntu_ver>/<arch>/..."
  exit 1
fi

# Map URL architecture to profile architecture
case "$URL_ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  *)
    echo "ERROR: Unknown architecture '$URL_ARCH' in URL. Expected x86_64 or aarch64."
    exit 1
    ;;
esac

# Extract Ubuntu version from URL
if [[ "$NEW_IMAGE_URL" =~ /images/([0-9]+\.[0-9]+)/ ]]; then
  UBUNTU_VER="${BASH_REMATCH[1]}"
else
  echo "ERROR: Cannot detect Ubuntu version from URL."
  exit 1
fi

# Extract build date from URL
if [[ "$NEW_IMAGE_URL" =~ /lvm/([0-9]{4}-[0-9]{2}-[0-9]{2})/ ]]; then
  BUILD_DATE="${BASH_REMATCH[1]}"
else
  echo "ERROR: Cannot detect build date from URL."
  exit 1
fi

# Derive short date (YYMMDD) for profile name
SHORT_DATE=$(echo "$BUILD_DATE" | sed 's/^20//' | tr -d '-')

# New profile name
NEW_PROFILE="nke-${SHORT_DATE}-${UBUNTU_VER}-${ARCH}-fde"

echo "Detected from URL:"
echo "  Architecture:   $URL_ARCH → $ARCH"
echo "  Ubuntu version: $UBUNTU_VER"
echo "  Build date:     $BUILD_DATE"
echo "  New profile:    $NEW_PROFILE"
echo ""

# --- Find current profile to replace ---
# Look for the existing active FDE profile matching this arch
CURRENT_PROFILE=""
for profile_dir in "$SCRIPT_DIR"/nke-*-${UBUNTU_VER}-${ARCH}-fde; do
  if [[ -d "$profile_dir" ]]; then
    candidate=$(basename "$profile_dir")
    if [[ "$candidate" != "$NEW_PROFILE" ]]; then
      CURRENT_PROFILE="$candidate"
    fi
  fi
done

if [[ -z "$CURRENT_PROFILE" ]]; then
  echo "ERROR: No existing active FDE profile found for ${UBUNTU_VER}-${ARCH}."
  echo "Looked for: nke-*-${UBUNTU_VER}-${ARCH}-fde in $SCRIPT_DIR"
  exit 1
fi

echo "Current profile to deprecate: $CURRENT_PROFILE"
echo ""

# --- Check new profile doesn't already exist ---
if [[ -d "$SCRIPT_DIR/$NEW_PROFILE" ]]; then
  echo "ERROR: New profile directory already exists: $SCRIPT_DIR/$NEW_PROFILE"
  exit 1
fi

# --- Step 1: Copy profile directory ---
echo "Step 1: Copying $CURRENT_PROFILE → $NEW_PROFILE ..."
cp -r "$SCRIPT_DIR/$CURRENT_PROFILE" "$SCRIPT_DIR/$NEW_PROFILE"

# --- Step 2: Update image URL in ipxeScript ---
echo "Step 2: Updating image URL in ipxeScript ..."
IPXE_FILE="$SCRIPT_DIR/$NEW_PROFILE/ipxeScript"
if [[ ! -f "$IPXE_FILE" ]]; then
  echo "ERROR: ipxeScript not found in new profile"
  rm -rf "$SCRIPT_DIR/$NEW_PROFILE"
  exit 1
fi

# Replace the image-url line
if ! grep -q "^set image-url " "$IPXE_FILE"; then
  echo "ERROR: No 'set image-url' line found in ipxeScript"
  rm -rf "$SCRIPT_DIR/$NEW_PROFILE"
  exit 1
fi
sed -i.bak "s|^set image-url .*|set image-url ${NEW_IMAGE_URL}|" "$IPXE_FILE"
rm -f "${IPXE_FILE}.bak"

# --- Step 3: Move old profile to deprecated ---
echo "Step 3: Moving $CURRENT_PROFILE to deprecated/ ..."
mkdir -p "$DEPRECATED_DIR"
mv "$SCRIPT_DIR/$CURRENT_PROFILE" "$DEPRECATED_DIR/$CURRENT_PROFILE"

# --- Step 4: Update metadata.yaml (replace old entry with new) ---
echo "Step 4: Updating metadata.yaml ..."

# Extract the old profile's metadata before removing it
OLD_K8S_VER=$(yq eval ".profiles.\"$CURRENT_PROFILE\".kubernetes_version" "$METADATA_FILE")
OLD_K8S_INJECTED=$(yq eval ".profiles.\"$CURRENT_PROFILE\".kubernetes_version_injected" "$METADATA_FILE")
OLD_ARCH=$(yq eval ".profiles.\"$CURRENT_PROFILE\".architecture" "$METADATA_FILE")
OLD_BUILD_DATE=$(yq eval ".profiles.\"$CURRENT_PROFILE\".build_date" "$METADATA_FILE")
OLD_UBUNTU_VER=$(yq eval ".profiles.\"$CURRENT_PROFILE\".ubuntu_version" "$METADATA_FILE")

# Remove old entry from metadata.yaml
yq eval -i "del(.profiles.\"$CURRENT_PROFILE\")" "$METADATA_FILE"

# Add new entry to metadata.yaml
yq eval -i ".profiles.\"$NEW_PROFILE\".kubernetes_version = \"\"" "$METADATA_FILE"
yq eval -i ".profiles.\"$NEW_PROFILE\".kubernetes_version_injected = true" "$METADATA_FILE"
yq eval -i ".profiles.\"$NEW_PROFILE\".architecture = \"$ARCH\"" "$METADATA_FILE"
yq eval -i ".profiles.\"$NEW_PROFILE\".build_date = \"$BUILD_DATE\"" "$METADATA_FILE"
yq eval -i ".profiles.\"$NEW_PROFILE\".ubuntu_version = \"$UBUNTU_VER\"" "$METADATA_FILE"

# --- Step 5: Add old profile to deprecated.yaml ---
echo "Step 5: Adding $CURRENT_PROFILE to deprecated.yaml ..."

yq eval -i ".profiles.\"$CURRENT_PROFILE\".kubernetes_version = \"$OLD_K8S_VER\"" "$DEPRECATED_FILE"
if [[ "$OLD_K8S_INJECTED" == "true" ]]; then
  yq eval -i ".profiles.\"$CURRENT_PROFILE\".kubernetes_version_injected = true" "$DEPRECATED_FILE"
fi
yq eval -i ".profiles.\"$CURRENT_PROFILE\".architecture = \"$OLD_ARCH\"" "$DEPRECATED_FILE"
yq eval -i ".profiles.\"$CURRENT_PROFILE\".build_date = \"$OLD_BUILD_DATE\"" "$DEPRECATED_FILE"
yq eval -i ".profiles.\"$CURRENT_PROFILE\".ubuntu_version = \"$OLD_UBUNTU_VER\"" "$DEPRECATED_FILE"

# --- Done ---
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Done!"
echo "  New profile:        $NEW_PROFILE"
echo "  Deprecated profile: $CURRENT_PROFILE"
echo "  Image URL:          $NEW_IMAGE_URL"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Run 'bash validate-metadata.sh' to verify."
