# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Repoint the first-party deps at REGISTRY and pin CWE to the built version."""
import re, sys
path, registry, version = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
def sub(name, ver):
    global s
    pat = r'(\{name: %s, version: )"([^"]*)"(, repository: )"[^"]*"' % re.escape(name)
    # Unlimited, so a duplicate raises the count rather than hiding behind the
    # limit. A miss leaves the dependency on its committed placeholder version
    # and GHCR repository, which helm resolves as a real chart -- the wrong one;
    # renaming a published chart is exactly how the pattern stops matching.
    s, n = re.subn(pat, lambda m: '%s"%s"%s"%s"' % (m.group(1), ver or m.group(2), m.group(3), registry), s)
    if n != 1:
        sys.exit("nke-stack-retarget: expected 1 %s dependency in %s, found %d" % (name, path, n))
sub("nke-controller", version)
sub("spire", None)
open(path, "w").write(s)
