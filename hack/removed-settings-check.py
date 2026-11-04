#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fail when a removed name of a controller setting is back in the tree.

WHY THIS EXISTS. #1399 removed the CWE_* and NKE_CONTROLLER_* names of the
controller's development, test and build settings, deprecated aliases from
v0.23.0, together with the `cwe_image` Tilt key, the `--cwe-api` flag of
get-kubeconfig.sh and the `cwe-name` input of qualify-e2e.yml, and renamed
LOCAL_CWE_IMAGE (local-backend-e2e.sh). Nothing reads the old names now, so a
value set under one is ignored without a warning. A branch cut before the removal, or a script copied from one, can
bring a reader back, and then the old name works again in that one place only.
This check fails on any of those names in a tracked file.

The NVKE_CONTROLLER_* names, CONTROLLER_IMAGE, LOCAL_NVKE_CONTROLLER_IMAGE,
`nvke_controller_image`, `--controller-api` and `controller-name` are the
names to use.

WHAT IS NOT SCANNED. Untracked files, binary files, CHANGELOG.md (generated
from commit messages, which name the removal) and this check's own files.

ALLOWED lists a token, in one file, that is spelled like a removed name but is
not a controller setting. Write the reason next to each entry.

Exit status: 0 clean, 1 a removed name is back, 2 the check could not run.
"""

import argparse
import re
import subprocess
import sys
from pathlib import Path

OWN_FILES = ("hack/removed-settings-check.py", "hack/test_removed_settings_check.py")
EXCLUDED_FILES = ("CHANGELOG.md",)

# The suffixes that the CWE_ and NKE_CONTROLLER_ prefixes carried.
SUFFIXES = (
    "IMAGE|BUILD_VERSION|BIN_NAME|HTTP_PORT|TEMPORAL_UI_PORT|NAMESPACE|SERVICE|SERVICE_PORT|API|API_URL"
    "|HEALTH_TIMEOUT|EXPECTED_VERSION|PROFILES_DIR|PROFILES_PRELOADED|RELEASES_DIR|GOFAIL_E2E_[A-Z0-9_]*"
)
REMOVED = re.compile(
    rf"(?<![A-Za-z0-9_])(?:CWE|NKE_CONTROLLER)_(?:{SUFFIXES})(?![A-Za-z0-9_])"
    r"|(?<![A-Za-z0-9_])LOCAL_CWE_IMAGE(?![A-Za-z0-9_])"
    r"|(?<![A-Za-z0-9_])cwe_image(?![A-Za-z0-9_])"
    r"|(?<![A-Za-z0-9_-])--cwe-api(?![A-Za-z0-9_-])"
    r"|(?<![A-Za-z0-9_-])cwe-name(?![A-Za-z0-9_-])"
)

ALLOWED = {
    # The script's own variable for its --cwe-api-url flag, not a controller
    # setting. The script was not part of #1399.
    ("controller/scripts/cwe-to-platform-crs.sh", "CWE_API_URL"),
    # A test that asserts nvkectl's bootstrap does not set the old name.
    ("controller/test/e2e/internal/site/spec_test.go", "CWE_IMAGE"),
}


def die(message):
    print(f"removed-settings-check: {message}", file=sys.stderr)
    sys.exit(2)


def tracked_files(root):
    try:
        out = subprocess.run(["git", "-C", str(root), "ls-files", "-z"], check=True, capture_output=True).stdout
    except (OSError, subprocess.CalledProcessError) as err:
        die(f"git ls-files failed, so nothing was scanned: {err}")
    return [p for p in out.decode("utf-8", "surrogateescape").split("\0") if p]


def main(argv=None):
    parser = argparse.ArgumentParser(description="Fail when a removed name of a controller setting is back in the tree.")
    parser.add_argument("--root", default=None, help="repository root (default: the parent of this script's directory)")
    args = parser.parse_args(argv)
    root = Path(args.root).resolve() if args.root else Path(__file__).resolve().parent.parent

    paths = [p for p in tracked_files(root) if p not in OWN_FILES and p.split("/")[-1] not in EXCLUDED_FILES]
    hits = []
    for path in paths:
        full = root / path
        if not full.is_file():
            continue
        data = full.read_bytes()
        if b"\0" in data[:8192]:
            continue
        for number, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
            for match in REMOVED.finditer(line):
                if (path, match.group(0)) not in ALLOWED:
                    hits.append(f"{path}:{number}: {match.group(0)}")

    if hits:
        for hit in hits:
            print(f"  {hit}")
        print(
            f"removed-settings-check: {len(hits)} removed setting names (#1399); use the NVKE_CONTROLLER_* name, "
            "CONTROLLER_IMAGE, LOCAL_NVKE_CONTROLLER_IMAGE, nvke_controller_image, --controller-api or controller-name",
            file=sys.stderr,
        )
        return 1
    print(f"removed-settings-check: clean ({len(paths)} tracked files)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
