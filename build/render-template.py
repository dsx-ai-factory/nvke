# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Render a template item's .yaml.tmpl files against a BootstrapConfig.

  render-template.py <config.json> <item-dir> <out-dir>

docs/site-bootstrap/deploy-repository.md defines `type: template` items: the
deploy tooling renders each .yaml.tmpl with Go text/template, passing the full
BootstrapConfig as the context, and the item is then applied as a manifest.
That tooling is nkectl, and it does not exist yet -- there is no BootstrapConfig
type in this repository, only the documented shape. This stands in until there
is, and is deleted with build/use-nke-sbom.sh when nkectl takes over.
Tracked in https://github.com/dsx-ai-factory/nke/issues/873.

The context is JSON rather than YAML because PyYAML is not guaranteed on the
e2e runners; the caller writes it, and its field names are BootstrapConfig's
(.Registry.NGCAPIKey, .Database.SuperuserPassword, ...).

DELIBERATELY A SUBSET. Only `{{ .Path.To.Field }}` is supported. Go's range, if,
with, template and function calls are rejected by name rather than silently
mis-rendered, because this is a stand-in and a half-rendered Secret is worse
than a refused one -- the doc's own reference templates range over
.Cluster.Nodes, and those need the real thing.

Output goes to a separate directory, never in place: the item may be a read-only
extraction of a published artifact, and what comes out holds credentials.
"""
import json
import os
import re
import shutil
import sys

FIELD = re.compile(r'\{\{-?\s*\.([A-Za-z_][\w.]*)\s*-?\}\}')
ANY_ACTION = re.compile(r'\{\{-?\s*(.*?)\s*-?\}\}', re.S)


def die(msg):
    sys.exit("render-template: %s" % msg)


def lookup(ctx, path, where):
    node = ctx
    for part in path.split("."):
        if not isinstance(node, dict) or part not in node:
            die("%s: {{ .%s }} has no value in the config.\n"
                "The context is the BootstrapConfig; add the field or correct the template."
                % (where, path))
        node = node[part]
    if isinstance(node, (dict, list)):
        die("%s: {{ .%s }} resolves to a %s, not a scalar."
            % (where, path, type(node).__name__))
    # A missing value renders as empty and fails three phases later, as a Secret
    # with a blank password. Refuse here instead -- this is Go's missingkey=error.
    if node is None or node == "":
        die("%s: {{ .%s }} is empty." % (where, path))
    return str(node)


def render(text, ctx, where):
    for action in ANY_ACTION.findall(text):
        if not action.startswith("."):
            die("%s: unsupported template action {{ %s }}.\n"
                "This renderer handles field lookups only; range, if, with and "
                "functions need nkectl's implementation." % (where, action))
    return FIELD.sub(lambda m: lookup(ctx, m.group(1), where), text)


if len(sys.argv) != 4:
    sys.exit("usage: render-template.py <config.json> <item-dir> <out-dir>")
config_path, src, out = sys.argv[1:]

try:
    ctx = json.load(open(config_path))
except (OSError, ValueError) as err:
    die("reading %s: %s" % (config_path, err))

os.makedirs(out, exist_ok=True)
rendered = 0
for name in sorted(os.listdir(src)):
    path = os.path.join(src, name)
    if not os.path.isfile(path):
        continue
    if name.endswith(".yaml.tmpl"):
        text = render(open(path).read(), ctx, name)
        if not text.strip():
            die("%s: rendered to nothing." % name)
        open(os.path.join(out, name[:-len(".tmpl")]), "w").write(text)
        rendered += 1
    elif name.endswith((".yaml", ".yml")):
        shutil.copy(path, os.path.join(out, name))
    # Anything else is ignored, matching the documented item layout.

if rendered == 0:
    die("%s holds no .yaml.tmpl files; it is a manifest item, not a template one." % src)
print(out)
