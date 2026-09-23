# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""List the namespaces nke-stack places resources into, read from values.yaml.

helm --create-namespace creates only the release namespace, so every namespace
a component is relocated into with namespaceOverride/namespace has to exist
before install. Reading them out of values.yaml keeps this from drifting.
"""
import re
import sys

text = open(sys.argv[1]).read()
seen = []
for m in re.finditer(r'^\s*(?:namespaceOverride|namespace):\s*([A-Za-z0-9][A-Za-z0-9-]*)\s*$',
                     text, re.M):
    if m.group(1) not in seen:
        seen.append(m.group(1))
print("\n".join(seen))
