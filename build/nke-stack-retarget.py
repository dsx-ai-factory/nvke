"""Repoint the first-party deps at REGISTRY and pin CWE to the built version."""
import re, sys
path, registry, version = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
def sub(name, ver):
    global s
    pat = r'(\{name: %s, version: )"([^"]*)"(, repository: )"[^"]*"' % re.escape(name)
    s = re.sub(pat, lambda m: '%s"%s"%s"%s"' % (m.group(1), ver or m.group(2), m.group(3), registry), s, count=1)
sub("controller", version)
sub("spire", None)
open(path, "w").write(s)
