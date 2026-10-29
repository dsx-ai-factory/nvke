# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Render a template item's .yaml.tmpl files against a BootstrapConfig.

  render-template.py <config.json> <item-dir> <out-dir>

docs/site-bootstrap/deploy-repository.md defines `type: template` items: the
deploy tooling renders each .yaml.tmpl with Go text/template, passing the full
BootstrapConfig as the context, and the item is then applied as a manifest.
There is no BootstrapConfig type in this repository, only the documented
shape, so deploy/nvke-deploy.sh renders with this script.

The context is JSON rather than YAML because PyYAML is not guaranteed on the
e2e runners; the caller writes it, and its field names are BootstrapConfig's
(.Registry.NGCAPIKey, .Database.SuperuserPassword, ...).

DELIBERATELY A SUBSET. `{{ .Path.To.Field }}` and a one-level
`{{ if .Path }}` / `{{ else }}` / `{{ end }}` are supported. Go's range, with,
template and function calls are rejected by name rather than silently
mis-rendered, because this is a stand-in and a half-rendered Secret is worse
than a refused one -- the doc's own reference templates range over
.Cluster.Nodes, and those need the real thing.

`if` is here because a credential has forms: CAPNICo takes either a static
token or OAuth client credentials and refuses a Secret carrying both, so the
one template has to emit one set or the other. Only a field's truthiness is
tested, and the branch not taken is dropped BEFORE any lookup -- otherwise the
empty field that made the branch false would itself be fatal.

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
# One level, no nesting: the body of each branch may not itself hold an if.
# Every marker's dashes are captured because `{{-` trims the whitespace before
# it and `-}}` the whitespace after, and a renderer that drops that leaves
# stray blank lines -- which is the silent mis-rendering this file refuses to
# do elsewhere.
IF_BLOCK = re.compile(
    r'\{\{(?P<d1>-?)\s*if\s+\.([A-Za-z_][\w.]*)\s*(?P<d2>-?)\}\}'
    r'(?P<then>((?!\{\{-?\s*if\s).)*?)'
    r'(\{\{(?P<d3>-?)\s*else\s*(?P<d4>-?)\}\}(?P<else>((?!\{\{-?\s*if\s).)*?))?'
    r'\{\{(?P<d5>-?)\s*end\s*(?P<d6>-?)\}\}',
    re.S)


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


def truthy(ctx, path, where):
    """Whether `if .path` takes its first branch.

    An absent field is false, unlike a lookup, which refuses one. The
    difference is deliberate: `if` asks whether something is configured, and a
    BootstrapConfig written before the field existed answers that honestly by
    not carrying it. Refusing here would mean every older config stopped
    rendering the moment a template learned to branch on a new form.

    An empty value is false for the same reason, which is the whole point of
    testing it.
    """
    node = ctx
    for part in path.split("."):
        if not isinstance(node, dict) or part not in node:
            return False
        node = node[part]
    if isinstance(node, (dict, list)):
        die("%s: {{ if .%s }} tests a %s, not a scalar."
            % (where, path, type(node).__name__))
    return node is not None and node != "" and node is not False


def resolve_ifs(text, ctx, where):
    """Drop the branch not taken, innermost-first.

    Repeated until no block is left so that two sibling ifs both resolve.
    A nested if is not matched at the outer position, because the then
    group refuses one: the inner block resolves first and the outer
    follows. Nesting is not refused, only reordered, and each pass removes
    one block, so this terminates.
    """
    while True:
        match = IF_BLOCK.search(text)
        if match is None:
            return text
        took_then = truthy(ctx, match.group(2), where)
        if took_then:
            kept = match.group("then")
            # The marker that closes this branch is `else` when there is one.
            closing = match.group("d3") if match.group("else") is not None else match.group("d5")
            opening = match.group("d2")
        else:
            kept = match.group("else") or ""
            closing = match.group("d5")
            opening = match.group("d4") or ""
        if opening:
            kept = kept.lstrip()
        if closing:
            kept = kept.rstrip()
        left, right = text[:match.start()], text[match.end():]
        if match.group("d1"):
            left = left.rstrip()
        if match.group("d6"):
            right = right.lstrip()
        text = left + kept + right


def render(text, ctx, where):
    text = resolve_ifs(text, ctx, where)
    for action in ANY_ACTION.findall(text):
        if not action.startswith("."):
            die("%s: unsupported template action {{ %s }}.\n"
                "This renderer handles field lookups and a one-level if/else; "
                "range, with and functions need nvkectl's implementation." % (where, action))
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
