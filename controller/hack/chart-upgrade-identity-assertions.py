#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert that upgrading a pre-rename install keeps every object it already runs.

chart-identity-assertions.py renders the chart three ways and compares names.
That is a check on templates. This is a check on a CLUSTER: an install of the
pre-rename chart, upgraded in place by the current chart with the values a site
pins, and then compared against itself.

The two catch different things, which is why both exist. `helm template` cannot
see a uid, cannot see a ReplicaSet, and cannot see the API server reject an
immutable field -- and `spec.selector` on a Deployment is immutable, so a rename
that slips past the template check surfaces as a failed upgrade on a real site,
or worse as an orphaned Deployment next to a new one.

Two modes:

  snapshot  record what the release owns right now: every object in
            `helm get manifest`, looked up live for its uid and the fields a
            rename would move. --include-from adds the objects a PRIOR snapshot
            held, so an object the new chart dropped is recorded as absent
            rather than going unnoticed.

  compare   read a before and an after snapshot and assert the upgrade moved
            nothing.

What compare asserts, and why each one is here:

  * One release, same name and namespace, revision advanced. Two releases means
    the "upgrade" was a second install standing beside the first.
  * Every object the before snapshot held still exists, with the SAME uid. A
    recreated object is as bad as a renamed one: it was deleted first, and
    whatever it carried went with it.
  * No object in the after snapshot is a rename twin of a before object -- the
    same name with one of RENAME_PAIRS swapped. This is the orphan signature:
    the old object left behind and a new one beside it.
  * The Deployment's uid, spec.selector.matchLabels and container names are
    unchanged, and it still owns every ReplicaSet it owned before. A rolling
    update adds one and is reported, not failed -- that is how an upgrade
    works. Losing the old ones is a replacement, which is what a moved selector
    looks like from the outside.
  * The SPIFFE subjects the nke-rbac ConfigMap names are unchanged. They are
    what SPIRE issues the controller and what the tenant clusters bind; moving
    one locks the controller out of every tenant it manages.

DOCUMENTED_GROUP_MIGRATION below is the one place a difference is tolerated,
and it is tolerated loudly.
"""

import argparse
import json
import pathlib
import re
import subprocess
import sys

import yaml

# The kinds the chart renders. A kind missing here is a kind this check is blind
# to, so add one when the chart gains one -- compare() fails on an object whose
# kind it cannot look up rather than skipping it.
NAMESPACED = {
    "ConfigMap",
    "Deployment",
    "NetworkPolicy",
    "Role",
    "RoleBinding",
    "Secret",
    "Service",
    "ServiceAccount",
    "ServiceMonitor",
}
CLUSTER_SCOPED = {
    "ClusterRole",
    "ClusterRoleBinding",
    "CustomResourceDefinition",
    "Namespace",
}

# The substitutions the rename makes. compare() applies each one, in both
# directions, to every before-name and fails if the result is a NEW object in
# the after snapshot -- that pair is the orphan signature the lane exists to
# catch.
RENAME_PAIRS = (
    ("nke", "nvke"),
    ("cloud-workflow-engine", "nvke-controller"),
    ("cwe", "nvke-controller"),
)

# The one difference the upgrade is allowed to have, and the reason.
#
# The management-engine CRD group moved from internal.nke.nvidia.com to
# internal.nvke.nvidia.com. A CRD group cannot be renamed -- the new group is a
# new CRD -- so #1496 made it a documented migration rather than a pinnable
# name: a site copies its ClusterRelease, MachineImage and NodePoolRelease CRs
# across.
#
# After the upgrade a site holds BOTH groups. The old CRDs carry
# `helm.sh/resource-policy: keep` (templates/crds.yaml, from the generated
# files/crds.yaml), so Helm leaves them behind rather than deleting them when
# they drop out of the manifest -- which is what keeps the CRs intact while the
# copy is made. By the letter of the check that is an orphan next to a renamed
# twin, which is exactly the shape this lane exists to catch; it is listed here
# so that the ONE instance that is intended does not drown out the rest, and
# compare() still prints it rather than passing in silence.
#
# Listed by exact name on purpose: a fourth CRD appearing under the new group,
# or one of these going missing, is not covered and still fails.
DOCUMENTED_GROUP_MIGRATION = {
    ("CustomResourceDefinition", None, "clusterreleases.internal.nvke.nvidia.com"),
    ("CustomResourceDefinition", None, "machineimages.internal.nvke.nvidia.com"),
    ("CustomResourceDefinition", None, "nodepoolreleases.internal.nvke.nvidia.com"),
}


def run(cmd: list[str]) -> str:
    """Run cmd and return its stdout, raising with the stderr on failure."""
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)} failed: {proc.stderr.strip()}")
    return proc.stdout


def manifest_objects(release: str, namespace: str, kubeconfig: str | None) -> list[tuple]:
    """Return (kind, namespace, name) for every object `helm get manifest` lists.

    Hooks are not in the manifest, which is what we want: the chart's test Pod
    is created by `helm test` and deleted again, and is not an identity a site
    carries.
    """
    cmd = ["helm", "get", "manifest", release, "--namespace", namespace]
    if kubeconfig:
        cmd += ["--kubeconfig", kubeconfig]
    out = []
    for doc in yaml.safe_load_all(run(cmd)):
        if not doc or "kind" not in doc:
            continue
        meta = doc.get("metadata") or {}
        kind = doc["kind"]
        # The manifest's namespace field is not authoritative for a namespaced
        # object the chart left unqualified -- Helm applies those into the
        # release namespace. Resolve it the same way.
        ns = meta.get("namespace") or (namespace if kind in NAMESPACED else None)
        out.append((kind, ns, meta.get("name")))
    return out


def kubectl_get(kind: str, ns: str | None, name: str, kubeconfig: str | None) -> dict | None:
    """Return the live object as a dict, or None when it does not exist."""
    cmd = ["kubectl", "get", kind, name, "-o", "json"]
    if ns:
        cmd += ["--namespace", ns]
    if kubeconfig:
        cmd += ["--kubeconfig", kubeconfig]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        if "NotFound" in proc.stderr or "not found" in proc.stderr:
            return None
        raise RuntimeError(f"{' '.join(cmd)} failed: {proc.stderr.strip()}")
    return json.loads(proc.stdout)


def replica_sets(deployment: dict, kubeconfig: str | None) -> list[dict]:
    """Return the ReplicaSets the Deployment owns, as {name, uid}, sorted.

    Ownership is by uid rather than by label selector on purpose: a rename that
    moved the selector would change which ReplicaSets the selector matches, and
    the check would then compare two different sets and call them equal.
    """
    ns = deployment["metadata"]["namespace"]
    uid = deployment["metadata"]["uid"]
    cmd = ["kubectl", "get", "replicasets", "--namespace", ns, "-o", "json"]
    if kubeconfig:
        cmd += ["--kubeconfig", kubeconfig]
    items = json.loads(run(cmd))["items"]
    owned = [
        {"name": rs["metadata"]["name"], "uid": rs["metadata"]["uid"]}
        for rs in items
        if any(ref.get("uid") == uid for ref in rs["metadata"].get("ownerReferences") or [])
    ]
    return sorted(owned, key=lambda r: r["name"])


def spiffe_subjects(config_map: dict) -> list[str]:
    """Return the spiffe:// subjects of the ClusterRoleBindings the nke-rbac ConfigMap carries."""
    manifest = (config_map.get("data") or {}).get("manifest.yaml")
    if not manifest:
        return []
    subjects = {
        s["name"]
        for d in yaml.safe_load_all(manifest)
        if d and d.get("kind") == "ClusterRoleBinding"
        for s in d.get("subjects") or []
        if str(s.get("name", "")).startswith("spiffe://")
    }
    return sorted(subjects)


def key(entry: dict) -> tuple:
    """The identity of a snapshot entry: what it is and where it lives, never its uid."""
    return (entry["kind"], entry["namespace"], entry["name"])


def snapshot(args: argparse.Namespace) -> int:
    """Record the release and every object it owns, live."""
    cmd = ["helm", "list", "--all-namespaces", "-o", "json"]
    if args.kubeconfig:
        cmd += ["--kubeconfig", args.kubeconfig]
    # Scoped to the release under test and anything that could be a rename of
    # it. Listing every release in the cluster would let an unrelated one --
    # the caller's, on a cluster this did not create -- fail the "exactly one
    # release" check; leaving the twins out would miss the failure that check
    # is for, a second release installed beside the first under the new name.
    interesting = {args.release} | rename_twins(args.release)
    releases = [
        {"name": r["name"], "namespace": r["namespace"], "revision": r["revision"], "status": r["status"], "chart": r["chart"]}
        for r in json.loads(run(cmd))
        if r["name"] in interesting
    ]

    wanted = set(manifest_objects(args.release, args.namespace, args.kubeconfig))
    if args.include_from:
        prior = json.loads(pathlib.Path(args.include_from).read_text())
        wanted |= {tuple(e["key"]) if "key" in e else key(e) for e in prior["objects"]}

    objects = []
    for kind, ns, name in sorted(wanted, key=lambda t: (t[0], t[1] or "", t[2])):
        if kind not in NAMESPACED and kind not in CLUSTER_SCOPED:
            print(f"snapshot: {kind} is in neither NAMESPACED nor CLUSTER_SCOPED; add it", file=sys.stderr)
            return 1
        live = kubectl_get(kind, ns, name, args.kubeconfig)
        entry = {"kind": kind, "namespace": ns, "name": name, "present": live is not None}
        if live is not None:
            entry["uid"] = live["metadata"]["uid"]
            if kind == "Deployment":
                entry["selector"] = live["spec"]["selector"]["matchLabels"]
                entry["containers"] = [c["name"] for c in live["spec"]["template"]["spec"]["containers"]]
                entry["replicaSets"] = replica_sets(live, args.kubeconfig)
            if kind == "ConfigMap" and name == "nke-rbac":
                entry["spiffeSubjects"] = spiffe_subjects(live)
        objects.append(entry)

    out = {"release": args.release, "namespace": args.namespace, "releases": releases, "objects": objects}
    pathlib.Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True) + "\n")
    print(f"wrote {args.out}: {len(objects)} objects, {len(releases)} release(s)")
    return 0


def rename_twins(name: str) -> set[str]:
    """Every name `name` could have become under the rename, in both directions."""
    out = set()
    for old, new in RENAME_PAIRS:
        # Word-ish boundaries: 'nke' must not match inside 'nvke'.
        out.add(re.sub(rf"(?<![a-z]){re.escape(old)}(?![a-z])", new, name))
        out.add(re.sub(rf"(?<![a-z]){re.escape(new)}(?![a-z])", old, name))
    return {n for n in out if n != name}


def compare(args: argparse.Namespace) -> int:
    """Assert the after snapshot holds everything the before snapshot did, unmoved."""
    before = json.loads(pathlib.Path(args.before).read_text())
    after = json.loads(pathlib.Path(args.after).read_text())
    failures: list[str] = []

    before_by_key = {key(e): e for e in before["objects"] if e["present"]}
    after_by_key = {key(e): e for e in after["objects"] if e["present"]}

    # One release, advanced, not two.
    if len(after["releases"]) != 1:
        failures.append(
            f"expected exactly one Helm release after the upgrade, found "
            f"{[(r['name'], r['namespace']) for r in after['releases']]}. "
            "More than one means the upgrade installed beside the old release rather than over it"
        )
    else:
        rel = after["releases"][0]
        was = before["releases"][0] if before["releases"] else {}
        if (rel["name"], rel["namespace"]) != (was.get("name"), was.get("namespace")):
            failures.append(
                f"the release moved from {was.get('name')!r} in {was.get('namespace')!r} "
                f"to {rel['name']!r} in {rel['namespace']!r}; a Helm release cannot be renamed in place"
            )
        if rel["status"] != "deployed":
            failures.append(f"release status is {rel['status']!r}, expected 'deployed'")
        if int(rel["revision"]) <= int(was.get("revision", 0)):
            failures.append(f"release revision did not advance: {was.get('revision')} -> {rel['revision']}")

    # Nothing the site already ran may be gone, and nothing may have been
    # recreated under the same name.
    for k, entry in sorted(before_by_key.items()):
        after_entry = after_by_key.get(k)
        if after_entry is None:
            failures.append(
                f"{k[0]} {k[2]}{' in ' + k[1] if k[1] else ''} is gone after the upgrade. "
                "The site ran this object; the upgrade orphaned or deleted it"
            )
            continue
        if after_entry.get("uid") != entry.get("uid"):
            failures.append(
                f"{k[0]} {k[2]}{' in ' + k[1] if k[1] else ''} was recreated "
                f"(uid {entry.get('uid')} -> {after_entry.get('uid')}). "
                "It was deleted and remade, so anything it carried is gone"
            )

    # Nothing new may be a rename of something old.
    before_names = {(k[0], k[2]) for k in before_by_key}
    for k in sorted(set(after_by_key) - set(before_by_key)):
        twins = rename_twins(k[2])
        matched = sorted(n for kind, n in before_names if kind == k[0] and n in twins)
        if not matched:
            # Not reassurance: an object can replace another without its name
            # being a token swap of it (`<release>-<nameOverride>` concatenates
            # rather than substitutes). What proves nothing was replaced is the
            # before-side loop above, which fails on anything that went missing.
            print(f"note: {k[0]} {k[2]} is new", file=sys.stderr)
        elif k in DOCUMENTED_GROUP_MIGRATION:
            print(
                f"note: {k[0]} {k[2]} is new beside {matched} -- the documented CRD group "
                "migration (#1496). The site's CRs must be copied across; nothing deletes the old group",
                file=sys.stderr,
            )
        else:
            failures.append(
                f"{k[0]} {k[2]} is new and is a rename of {matched}, which still exists. "
                "This is the orphan signature: the pinned values did not hold this name"
            )

    # The Deployment is the one whose selector the API server will not let move.
    deployments = [e for e in before["objects"] if e["kind"] == "Deployment" and e["present"]]
    if not deployments:
        failures.append("the before snapshot holds no Deployment; the install did not take")
    for entry in deployments:
        k = key(entry)
        after_entry = after_by_key.get(k)
        if after_entry is None:
            continue  # already reported above
        if after_entry["selector"] != entry["selector"]:
            failures.append(
                f"Deployment {k[2]} selector changed from {entry['selector']} to {after_entry['selector']}. "
                "spec.selector is immutable; a real site's upgrade fails here"
            )
        if after_entry["containers"] != entry["containers"]:
            failures.append(
                f"Deployment {k[2]} containers changed from {entry['containers']} to {after_entry['containers']}. "
                "Fleet dashboards select on the container name"
            )
        # A ROLLING update is not a failure, and asking for "no rollout" would
        # make this lane unrunnable: the pinned chart is months of unrelated
        # change behind HEAD, and any of it that touches the pod template --
        # image tag, a mount path, an env var -- rolls the Deployment. That is
        # how a controller upgrade is supposed to work.
        #
        # What must not happen is a REPLACEMENT: the old ReplicaSets reparented
        # or dropped, which is what a moved selector looks like from here. The
        # uid check above covers the Deployment itself; this covers its history.
        lost = [rs for rs in entry["replicaSets"] if rs not in after_entry["replicaSets"]]
        if lost:
            failures.append(
                f"Deployment {k[2]} no longer owns ReplicaSets {lost}. "
                "They were deleted or reparented, which is what a moved selector looks like -- "
                "the Deployment was replaced rather than rolled"
            )
        gained = [rs["name"] for rs in after_entry["replicaSets"] if rs not in entry["replicaSets"]]
        if gained:
            print(
                f"note: Deployment {k[2]} rolled -- new ReplicaSet(s) {gained}, "
                "same Deployment, same selector. Expected on any upgrade that changes the pod template",
                file=sys.stderr,
            )

    # The identities SPIRE issues and the tenant clusters bind.
    #
    # The "was it there at all" case is separate from the "did it move" case on
    # purpose. A ConfigMap that is simply gone is already a failure from the
    # before-side loop, and reporting it a second time as "never compared"
    # reads like a mistake in the lane's own setup rather than the breakage it
    # just found.
    rbac_before = [e for e in before["objects"] if e["kind"] == "ConfigMap" and e["name"] == "nke-rbac" and e["present"]]
    if not rbac_before:
        failures.append(
            "the before snapshot holds no nke-rbac ConfigMap, so the SPIFFE subjects were never compared. "
            "chart-upgrade-before-values.yaml must set spiffe.trustDomain"
        )
    else:
        entry = rbac_before[0]
        after_entry = after_by_key.get(key(entry))
        if after_entry is not None and after_entry.get("spiffeSubjects") != entry.get("spiffeSubjects"):
            failures.append(
                f"nke-rbac SPIFFE subjects changed from {entry.get('spiffeSubjects')} "
                f"to {after_entry.get('spiffeSubjects')}. "
                "The tenant bindings would name an identity SPIRE no longer issues"
            )

    if failures:
        print(f"\nFAIL: the upgrade did not preserve the install ({len(failures)} problems)\n", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        print("", file=sys.stderr)
        return 1

    print(
        f"OK: {len(before_by_key)} objects kept their name and uid, the Deployment selector held, "
        "the SPIFFE subjects held"
    )
    return 0


# The identities the BEFORE install must be running for the upgrade to prove
# anything. CHART_UPGRADE_FROM_REF points at a chart whose own defaults rendered
# these; if someone bumps the ref to a chart that has already moved, every
# comparison below still passes and the lane silently tests nothing. These are
# the names from chart-identity-assertions.py's LEGACY scenario, which is the
# definition of "what a site that predates the rename runs".
REQUIRED_BASELINE = {
    ("Deployment", "cloud-workflow-engine"),
    ("Service", "cloud-workflow-engine"),
    ("ServiceAccount", "cloud-workflow-engine"),
    ("ServiceMonitor", "cloud-workflow-engine-metrics"),
    ("ConfigMap", "nke-controller-config"),
    ("ConfigMap", "nke-rbac"),
    ("Namespace", "nke-system"),
}


def check_baseline(args: argparse.Namespace) -> int:
    """Assert the before snapshot is a pre-rename install, not an already-renamed one."""
    before = json.loads(pathlib.Path(args.snapshot).read_text())
    present = {(e["kind"], e["name"]) for e in before["objects"] if e["present"]}
    missing = sorted(REQUIRED_BASELINE - present)
    failures = [f"{kind} {name}" for kind, name in missing]

    # The container name is the one the fleet dashboards select on, and it is
    # not an object name, so the set above cannot see it.
    for entry in before["objects"]:
        if entry["kind"] == "Deployment" and entry["present"]:
            if "cloud-workflow-engine" not in entry["containers"]:
                failures.append(f"Deployment container names {entry['containers']} (want cloud-workflow-engine)")
            break

    if failures:
        print(
            "\nFAIL: the chart at CHART_UPGRADE_FROM_REF is not a pre-rename chart.\n"
            "The upgrade would compare an already-renamed install against itself and pass\n"
            "without testing anything. Point CHART_UPGRADE_FROM_REF at a ref whose chart\n"
            "DEFAULTS render the legacy names.\n",
            file=sys.stderr,
        )
        for f in failures:
            print(f"  missing or moved: {f}", file=sys.stderr)
        print("", file=sys.stderr)
        return 1

    print(f"OK: the baseline install runs the legacy identities ({len(REQUIRED_BASELINE)} checked)")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="mode", required=True)

    snap = sub.add_parser("snapshot", help="record what the release owns right now")
    snap.add_argument("--release", required=True)
    snap.add_argument("--namespace", required=True)
    snap.add_argument("--out", required=True)
    snap.add_argument("--include-from", help="also look up every object this earlier snapshot held")
    snap.add_argument("--kubeconfig")
    snap.set_defaults(func=snapshot)

    base = sub.add_parser("check-baseline", help="assert the before snapshot is a pre-rename install")
    base.add_argument("--snapshot", required=True)
    base.set_defaults(func=check_baseline)

    cmp_ = sub.add_parser("compare", help="assert the upgrade moved nothing")
    cmp_.add_argument("--before", required=True)
    cmp_.add_argument("--after", required=True)
    cmp_.set_defaults(func=compare)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
