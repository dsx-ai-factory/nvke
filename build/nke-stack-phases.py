# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Emit one line per phase: name, cumulative --set flags, and preconditions.

Format: <name>\t<flags>\t<requires>
where <requires> is a space-separated list of kind/name/namespace/condition
tuples joined by ':' -- empty when the phase declares none.

Flags are cumulative: `helm upgrade` with a component disabled REMOVES it, so
toggling phase by phase would uninstall the previous phase's work.
"""
import re
import sys

chart_yaml, values_yaml = sys.argv[1], sys.argv[2]
deps = sorted(set(re.findall(r'\{name: ([A-Za-z0-9-]+),', open(chart_yaml).read())))

text = open(values_yaml).read()
block = re.search(r'^phases:\n((?:[ \t].*\n|\n)+?)(?=^\S)', text, re.M)
if not block:
    sys.exit("no phases: block in %s" % values_yaml)

# Split into per-phase chunks on "- name:" so requires: attaches to its phase.
chunks = re.split(r'^\s+- name:\s*', block.group(1), flags=re.M)[1:]

enabled = []
for chunk in chunks:
    name = chunk.split("\n", 1)[0].strip()
    comps = re.search(r'^\s+components:\s*\[([^\]]*)\]', chunk, re.M)
    enabled += [c.strip() for c in comps.group(1).split(",") if c.strip()] if comps else []
    flags = " ".join("--set %s.enabled=%s" % (d, "true" if d in enabled else "false")
                     for d in deps)

    reqs = []
    for req in re.finditer(
            r'-\s+kind:\s*(\S+)\s*\n\s+name:\s*(\S+)\s*\n\s+namespace:\s*(\S+)\s*\n\s+condition:\s*(\S+)',
            chunk):
        reqs.append(":".join(req.groups()))

    print("%s\t%s\t%s" % (name, flags, " ".join(reqs)))
