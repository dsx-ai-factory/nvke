# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Parse a generated deploy path into tab-separated lines a shell can read.

  items   <nvke-deploy.yaml>  -> "<name>\t<type>" per item, in listed order
  release <release.yaml>     -> "single\x1f<chart>\x1f<version>\x1f<repository>
                                \x1f<path>\x1f<release>\x1f<namespace>
                                \x1f<values>\x1f<requires>\x1f<artifact>
                                \x1f<imageArtifact>\x1f<profilesArtifact>
                                \x1f<setfile>", one line. artifact,
                                imageArtifact and profilesArtifact are the NVKR
                                artifacts the chart, the image it runs and its
                                profiles content image come from, by NVKR's
                                output names; each may be empty. <setfile> is
                                chart.setFile joined with commas: "<helm
                                key>=<absolute path>" entries the executor
                                passes as --set-file.

                                US-separated, not tab: bash collapses runs of tab
                                because it is an IFS whitespace character, so an
                                empty field in the middle would shift every field
                                after it -- which once put a values path in
                                --namespace.

PyYAML is not guaranteed on the e2e runners, which is why this is a regex parser
rather than a YAML load. items and release are strict -- any line they do not
recognise is a hard error, because a parser that skips what it does not
understand turns a malformed artifact into a deploy that silently does less than
it claims. phases reads one block out of a large hand-commented values.yaml, so
it is strict only within that block.

Used by deploy/nvke-deploy.sh, which lives one directory up.
"""
import re
import sys

ITEM = re.compile(r'^  - \{name: ([\w.-]+), type: (template|manifest|helm|argocd)\}$')
TOP = re.compile(r'^([\w]+):$')
KV = re.compile(r'^  ([\w]+): (\S+)$')
SEQ = re.compile(r'^  - (\S+)$')
SETFILE_HDR = re.compile(r'^  setFile:$')
SETFILE_ITEM = re.compile(r'^    - (\S+=/\S+)$')
FLOW_REQ = re.compile(r'^  - \{kind: ([\w.-]+), name: ([\w.-]+), '
                      r'namespace: ([\w.-]+), condition: ([\w.-]+)\}$')

PHASE_NAME = re.compile(r'^  - name: ([\w-]+)$')
COMPONENTS = re.compile(r'^    components: \[([^\]]*)\]$')
REQ_HDR = re.compile(r'^    requires:$')
REQ_KIND = re.compile(r'^      - kind: (\S+)$')
REQ_KV = re.compile(r'^        (name|namespace|condition): (\S+)$')


def unquote(pair):
    """Strip the surrounding quotes YAML uses to keep a version a string."""
    key, value = pair
    if len(value) > 1 and value[0] == value[-1] and value[0] in "\"'":
        value = value[1:-1]
    return key, value


def die(msg):
    sys.exit("nvke-deploy-items: %s" % msg)


def significant(path):
    """Yield (lineno, line) for everything that is not blank or a whole-line comment."""
    try:
        raw = open(path).read()
    except OSError as err:
        die(str(err))
    for num, line in enumerate(raw.splitlines(), 1):
        if line.strip() and not line.lstrip().startswith("#"):
            yield num, line


def items(path):
    header = False
    out = []
    for num, line in significant(path):
        if line == "items:":
            header = True
            continue
        match = ITEM.match(line)
        if not match or not header:
            die("%s:%d: unrecognised: %s" % (path, num, line))
        out.append("%s\t%s" % match.groups())
    if not header:
        die("%s: no items: key" % path)
    return out


def release(path):
    chart, rel, values, requires, setfile = {}, {}, [], [], []
    section = None
    in_setfile = False
    for num, line in significant(path):
        top = TOP.match(line)
        if top:
            section = top.group(1)
            if section not in ("chart", "release", "values", "requires"):
                die("%s:%d: unknown section: %s" % (path, num, section))
            continue
        if section is None:
            die("%s:%d: value before any section: %s" % (path, num, line))

        if section == "chart" and SETFILE_HDR.match(line):
            in_setfile = True
        elif section == "chart" and in_setfile and SETFILE_ITEM.match(line):
            setfile.append(SETFILE_ITEM.match(line).group(1))
        elif section == "chart" and KV.match(line):
            in_setfile = False
            chart.update([unquote(KV.match(line).groups())])
        elif section == "release" and KV.match(line):
            rel.update([unquote(KV.match(line).groups())])
        elif section == "values" and SEQ.match(line):
            values.append(SEQ.match(line).group(1))
        elif section == "requires" and FLOW_REQ.match(line):
            requires.append(":".join(FLOW_REQ.match(line).groups()))
        else:
            die("%s:%d: unrecognised: %s" % (path, num, line))

    if "name" not in chart:
        die("%s: missing chart.name" % path)
    # A repository chart is selected by its version; a path chart carries
    # its own.
    if not chart.get("version") and "path" not in chart:
        die("%s: missing chart.version for a repository chart" % path)
    chart.setdefault("version", "")
    if "namespace" not in rel:
        die("%s: missing release.namespace" % path)

    if "name" not in rel:
        die("%s: missing release.name" % path)
    if "repository" in chart and "path" in chart:
        die("%s: chart has both repository and path; name one source" % path)
    return ["single\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s" % (
        chart["name"], chart["version"], chart.get("repository", ""),
        chart.get("path", ""), rel["name"], rel["namespace"],
        ",".join(values), " ".join(requires), chart.get("artifact", ""),
        chart.get("imageArtifact", ""), chart.get("profilesArtifact", ""),
        ",".join(setfile))]


if len(sys.argv) != 3 or sys.argv[1] not in ("items", "release"):
    sys.exit("usage: nvke-deploy-items.py [items|release] <file>")
print("\n".join({"items": items, "release": release}[sys.argv[1]](sys.argv[2])))
