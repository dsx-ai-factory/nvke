#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Report every token in the tree that carries a banned name, outside an allowlist.

WHY THIS EXISTS. A name can stay in a tree long after the reason for it has
lapsed: a product name that was replaced, a domain the project does not own,
an identifier that moved. Hand sweeps do not converge. The sweeps that changed
"CWE" to "controller" landed no check, and hundreds of lines still say cwe. So
the tree needs one check that lists what is left and, once the list is meant
to be empty, fails on it.

TWO MODES. `--mode report` prints a summary (hits, distinct tokens, files, the
most common tokens) and exits 0, so the check can sit in the merge gate while
the tree still carries the name everywhere. `--mode fail` prints every hit
outside the allowlist and exits 1 when there is one. The make target chooses
the mode, so flipping it is a one-line change in make/checks.mk.

TOKENS, NOT LINES. A line that mentions the name can also carry an English
word that happens to contain it (`linked`, `chunked`, `blanket`) or a
camel-case identifier that does (`encryptionKey`), and a line-based stoplist
then hides the real token beside the harmless one: the label-key constants in
controller/internal/constants/constants.go sit on lines that also say
`AnnotationKey`. So the scan splits each line into tokens (runs of
[A-Za-z0-9_./:@-]), keeps the tokens that contain the name case-insensitively,
and then keeps a token only when the name sits where a product name would.
The character before it must be the start of the token, a separator
(_ - . / : @), a digit, or a lower-case letter in front of an upper-case name
(a camel-case boundary). The character after it must be the end of the token,
a separator, a digit, an upper-case letter after a lower-case or all-caps
name, a lower-case letter after an all-caps name, or the start of one of the
known product suffixes (`ctl`, `auth`, `rbac`, ...). That rule drops
`linked`, `encryptionKey`, `nats-io/nkeys` and the base64 runs in a sum file,
and keeps a package name, a label domain, a path under /etc and a CLI name.

PATHS TOO. A file or directory whose name carries the banned name is a hit of
its own, listed as a `path` hit, because a rename leaves those to `git mv`.

WHAT IS NOT SCANNED. Only tracked files (`git ls-files`), never untracked or
ignored ones. Excluded by path, as hack/docs-check.sh Check 7 does: sum and
lock files (content-addressed upstream artifacts), vendor/ and generated/
directories (third-party or regenerated), CHANGELOG.md (generated from
history, which is public anyway) and this check's own files, which have to
name what they forbid. Binary files are skipped.

THE ALLOWLIST. hack/name-check-allowlist.txt, one entry per line:
  path:<glob>    every hit in a file matching the glob is allowed, and so is
                 the path itself; `*` also matches `/`
  token:<regex>  every hit whose token matches the regex is allowed; Python
                 syntax, searched anywhere in the token, so anchor it
Blank lines and lines starting with # are comments; write the reason above
each entry. An entry that matches nothing is dead and fails the check in both
modes, so the allowlist cannot quietly outlive what it was for.

No ripgrep. GitHub-hosted runners do not have it, and a check that cannot run
has to say so rather than report a clean tree it never read. This one needs
python3 and git only.
"""

import argparse
import collections
import fnmatch
import re
import subprocess
import sys
from pathlib import Path

DEFAULT_NAME = "nke"
ALLOWLIST = "hack/name-check-allowlist.txt"
OWN_FILES = ("hack/name-check.py", "hack/name-check-allowlist.txt", "hack/test_name_check.py")
EXCLUDED_FILES = ("CHANGELOG.md",)
EXCLUDED_SUFFIXES = (".sum", ".lock")
EXCLUDED_DIRS = ("vendor", "generated")
# Lower-case continuations after a lower-case or capitalised name that make
# the token a product token rather than an English word.
PRODUCT_SUFFIXES = ("ctl", "proto", "utils", "auth", "rbac", "cluster", "luks", "k8s", "gpg", "cli", "nodepool")
TOKEN = re.compile(r"[A-Za-z0-9_./:@-]+")


def die(message):
    """A check that cannot run says so and exits 2; 1 is reserved for findings."""
    print(f"name-check: {message}", file=sys.stderr)
    sys.exit(2)


class Hit:
    __slots__ = ("path", "line", "token")

    def __init__(self, path, line, token):
        self.path, self.line, self.token = path, line, token

    def location(self):
        return f"{self.path}:{self.line}" if self.line else f"{self.path} (path)"


class Entry:
    __slots__ = ("kind", "value", "regex", "line", "used")

    def __init__(self, kind, value, regex, line):
        self.kind, self.value, self.regex, self.line, self.used = kind, value, regex, line, 0

    def allows(self, hit):
        if self.kind == "path":
            return fnmatch.fnmatchcase(hit.path, self.value)
        return self.regex.search(hit.token) is not None


def product_shaped(token, name_re):
    """True when the name sits in `token` where a product name would."""
    for match in name_re.finditer(token):
        start, end = match.span()
        found = match.group(0)
        pre = token[start - 1] if start > 0 else ""
        post = token[end] if end < len(token) else ""
        before = pre == "" or not pre.isalnum() or pre.isdigit() or (pre.islower() and found[0].isupper())
        if not before:
            continue
        if post == "" or not post.isalnum() or post.isdigit():
            return True
        if post.isupper() and (found.isupper() or found.islower() or found == found.capitalize()):
            return True
        if post.islower():
            if found.isupper():
                return True
            if found in (found.lower(), found.capitalize()) and token[end:].lower().startswith(PRODUCT_SUFFIXES):
                return True
    return False


def excluded(path):
    parts = path.split("/")
    if path in OWN_FILES or parts[-1] in EXCLUDED_FILES or parts[-1].endswith(EXCLUDED_SUFFIXES):
        return True
    return any(part in EXCLUDED_DIRS for part in parts[:-1])


def tracked_files(root):
    try:
        out = subprocess.run(["git", "-C", str(root), "ls-files", "-z"], check=True, capture_output=True).stdout
    except (OSError, subprocess.CalledProcessError) as err:
        die(f"git ls-files failed, so nothing was scanned: {err}")
    return [p for p in out.decode("utf-8", "surrogateescape").split("\0") if p]


def load_allowlist(path):
    entries = []
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as err:
        die(f"cannot read the allowlist: {err}")
    for number, raw in enumerate(lines, 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        kind, sep, value = line.partition(":")
        if kind not in ("path", "token") or not sep or not value:
            die(f"{path}:{number}: expected 'path:<glob>' or 'token:<regex>', got: {line}")
        regex = None
        if kind == "token":
            try:
                regex = re.compile(value)
            except re.error as err:
                die(f"{path}:{number}: bad regex {value!r}: {err}")
        entries.append(Entry(kind, value, regex, number))
    return entries


def scan(root, paths, name_re, name):
    hits, binary = [], 0
    lowered = name.lower()
    for path in paths:
        for part in path.split("/"):
            if lowered in part.lower() and product_shaped(part, name_re):
                hits.append(Hit(path, 0, part))
                break
        full = root / path
        if not full.is_file():
            continue
        data = full.read_bytes()
        if b"\0" in data[:8192]:
            binary += 1
            continue
        for number, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
            if lowered not in line.lower():
                continue
            for token in TOKEN.findall(line):
                if lowered in token.lower() and product_shaped(token, name_re):
                    hits.append(Hit(path, number, token))
    return hits, binary


def main(argv=None):
    parser = argparse.ArgumentParser(description="Report tokens that carry a banned name outside an allowlist.")
    parser.add_argument("--name", default=DEFAULT_NAME, help=f"the banned name (default: {DEFAULT_NAME})")
    parser.add_argument("--mode", choices=("report", "fail"), default="report", help="report summarises and exits 0; fail lists every hit and exits 1")
    parser.add_argument("--allowlist", default=None, help=f"allowlist file (default: {ALLOWLIST} under the repository root)")
    parser.add_argument("--root", default=None, help="repository root (default: the parent of this script's directory)")
    parser.add_argument("--top", type=int, default=20, help="how many tokens the report lists (default: 20)")
    args = parser.parse_args(argv)

    root = Path(args.root).resolve() if args.root else Path(__file__).resolve().parent.parent
    allowlist_path = Path(args.allowlist) if args.allowlist else root / ALLOWLIST
    name_re = re.compile(re.escape(args.name), re.IGNORECASE)
    entries = load_allowlist(allowlist_path)

    paths = tracked_files(root)
    scanned = [p for p in paths if not excluded(p)]
    hits, binary = scan(root, scanned, name_re, args.name)

    allowed, open_hits = 0, []
    for hit in hits:
        matched = False
        for entry in entries:
            if entry.allows(hit):
                entry.used += 1
                matched = True
        if matched:
            allowed += 1
        else:
            open_hits.append(hit)

    print(f"name-check: {root}")
    print(f"  name: {args.name} (case-insensitive, product-shaped tokens only)")
    print(f"  mode: {args.mode}")
    print(f"  scanned {len(scanned)} tracked files, {len(paths) - len(scanned)} excluded by path, {binary} binary skipped")
    content_hits = [h for h in hits if h.line]
    path_hits = [h for h in hits if not h.line]
    print(f"  hits: {len(content_hits)} in {len({h.path for h in content_hits})} files, {len(path_hits)} paths carry the name, {allowed} allowed by the allowlist")
    counts = collections.Counter(h.token for h in content_hits)
    print(f"  distinct tokens: {len(counts)}")
    if counts and args.mode == "report":
        print(f"  top {min(args.top, len(counts))} tokens:")
        for token, count in counts.most_common(args.top):
            print(f"    {count:6d}  {token}")

    status = 0
    dead = [e for e in entries if not e.used]
    for entry in dead:
        print(f"  DEAD  {allowlist_path}:{entry.line}: {entry.kind}:{entry.value} matches nothing; remove it", file=sys.stderr)
        status = 1
    if entries and not dead:
        print(f"  allowlist: {len(entries)} entries, all in use")

    if args.mode == "fail":
        for hit in sorted(open_hits, key=lambda h: (h.path, h.line, h.token)):
            print(f"  {hit.location()}: {hit.token}")
        if open_hits:
            print(f"name-check: {len(open_hits)} hits outside the allowlist", file=sys.stderr)
            status = 1
        elif status == 0:
            print("name-check: clean")
    elif status == 0:
        print("name-check: report only, exit 0")
    return status


if __name__ == "__main__":
    sys.exit(main())
