"""Parse a generated deploy path into tab-separated lines a shell can read.

  items   <nke-deploy.yaml>  -> "<name>\t<type>" per item, in listed order
  release <release.yaml>     -> "<chart>\t<version>\t<prefix>\t<namespace>\t<perPhase>\t<values>"
  phases  <values.yaml>      -> "<name>\t<components>\t<requires>" per phase, where
                                requires is a space-separated list of
                                kind:name:namespace:condition tuples

The phase list is read from the CHART's values.yaml, not from the deploy path:
the chart declares its own install order, and copying it into the SBOM would be
a second place for it to be wrong.

PyYAML is not guaranteed on the e2e runners, which is why this is a regex parser
rather than a YAML load. items and release are strict -- any line they do not
recognise is a hard error, because a parser that skips what it does not
understand turns a malformed artifact into a deploy that silently does less than
it claims. phases reads one block out of a large hand-commented values.yaml, so
it is strict only within that block.
"""
import re
import sys

ITEM = re.compile(r'^  - \{name: ([\w.-]+), type: ([\w-]+)\}$')
TOP = re.compile(r'^([\w]+):$')
KV = re.compile(r'^  ([\w]+): (\S+)$')
SEQ = re.compile(r'^  - (\S+)$')

PHASE_NAME = re.compile(r'^  - name: ([\w-]+)$')
COMPONENTS = re.compile(r'^    components: \[([^\]]*)\]$')
REQ_HDR = re.compile(r'^    requires:$')
REQ_KIND = re.compile(r'^      - kind: (\S+)$')
REQ_KV = re.compile(r'^        (name|namespace|condition): (\S+)$')


def die(msg):
    sys.exit("nke-sbom-items: %s" % msg)


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
    chart, rel, values = {}, {}, []
    section = None
    for num, line in significant(path):
        top = TOP.match(line)
        if top:
            section = top.group(1)
            if section not in ("chart", "release", "values"):
                die("%s:%d: unknown section: %s" % (path, num, section))
            continue
        if section is None:
            die("%s:%d: value before any section: %s" % (path, num, line))

        if section == "chart" and KV.match(line):
            chart.update([KV.match(line).groups()])
        elif section == "release" and KV.match(line):
            rel.update([KV.match(line).groups()])
        elif section == "values" and SEQ.match(line):
            values.append(SEQ.match(line).group(1))
        else:
            die("%s:%d: unrecognised: %s" % (path, num, line))

    for key, holder in (("name", chart), ("version", chart),
                        ("prefix", rel), ("namespace", rel), ("perPhase", rel)):
        if key not in holder:
            die("%s: missing %s" % (path, key))

    return ["%s\t%s\t%s\t%s\t%s\t%s" % (chart["name"], chart["version"], rel["prefix"],
                                        rel["namespace"], rel["perPhase"], ",".join(values))]


def phases(path):
    """Parse the phases: block of the chart's values.yaml."""
    try:
        raw = open(path).read()
    except OSError as err:
        die(str(err))
    # The block runs to the next line that starts in column 0.
    block = re.search(r'^phases:\n((?:[ \t].*\n|\n)+?)(?=^\S)', raw, re.M)
    if not block:
        die("%s: no phases: block" % path)

    found = []
    for line in block.group(1).splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if PHASE_NAME.match(line):
            found.append([PHASE_NAME.match(line).group(1), "", [], {}])
        elif not found:
            die("%s: %r appears before any phase name" % (path, line))
        elif COMPONENTS.match(line):
            found[-1][1] = ",".join(c.strip() for c in COMPONENTS.match(line).group(1).split(",") if c.strip())
        elif REQ_HDR.match(line):
            continue
        elif REQ_KIND.match(line):
            found[-1][3] = {"kind": REQ_KIND.match(line).group(1)}
        elif REQ_KV.match(line):
            key, value = REQ_KV.match(line).groups()
            if not found[-1][3]:
                die("%s: %r has no kind" % (path, line))
            found[-1][3][key] = value
            if key == "condition":
                req = found[-1][3]
                found[-1][2].append(":".join(req[k] for k in ("kind", "name", "namespace", "condition")))
                found[-1][3] = {}
        else:
            die("%s: unrecognised line in phases: %r" % (path, line))

    if not found:
        die("%s: phases: block declares no phases" % path)
    return ["%s\t%s\t%s" % (name, components, " ".join(reqs)) for name, components, reqs, _ in found]


if len(sys.argv) != 3 or sys.argv[1] not in ("items", "release", "phases"):
    sys.exit("usage: nke-sbom-items.py [items|release|phases] <file>")
print("\n".join({"items": items, "release": release, "phases": phases}[sys.argv[1]](sys.argv[2])))
