#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert that the chart still renders the deployed identities it always has.

The chart package is named `controller`, but every name it renders stays
`cloud-workflow-engine`, pinned by `nameOverride` in values.yaml. That split is
deliberate and permanent, not a migration step: existing sites keep the legacy
runtime naming indefinitely, and only new site installs opt into `controller`
naming. So "default values render the legacy names" is a durable invariant.

It is also easy to break by accident. Dropping or emptying `nameOverride`
renames the container, the ServiceAccount that SPIFFE identities derive from,
and the selector labels -- and `helm lint`, `helm template` and kubeconform all
still pass. Fleet dashboards select on `container="cloud-workflow-engine"`, and
a Deployment's `spec.selector` is immutable once applied, so an upgrade would
fail rather than rename.

Not asserted: `helm.sh/chart` and the `# Source:` comments. Both encode the
chart's own name by design, both changed when the package was renamed, and
neither is a selector or a consumer-visible identity.
"""

import argparse
import subprocess
import sys

import yaml

# The Helm release name production uses. `app.kubernetes.io/instance` and the
# fullname helper both derive from it, so rendering under any other release
# name would not exercise the identities that actually ship.
RELEASE = "cloud-workflow-engine"

EXPECTED_NAMES = {
    ("Namespace", "nke-system"),
    ("NetworkPolicy", "cloud-workflow-engine-metrics"),
    ("ServiceAccount", "cloud-workflow-engine"),
    ("Role", "cloud-workflow-engine"),
    ("Role", "cloud-workflow-engine-nke-rbac"),
    ("RoleBinding", "cloud-workflow-engine"),
    ("RoleBinding", "cloud-workflow-engine-nke-rbac"),
    ("Service", "cloud-workflow-engine"),
    ("Deployment", "cloud-workflow-engine"),
    ("ServiceMonitor", "cloud-workflow-engine-metrics"),
    ("Pod", "cloud-workflow-engine-test-connection"),
}

EXPECTED_CONTAINER = "cloud-workflow-engine"
EXPECTED_SELECTOR = {
    "app.kubernetes.io/name": "cloud-workflow-engine",
    "app.kubernetes.io/instance": "cloud-workflow-engine",
}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--chart", required=True)
    args = parser.parse_args()

    rendered = subprocess.run(
        ["helm", "template", RELEASE, args.chart],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    docs = [d for d in yaml.safe_load_all(rendered) if d]

    failures = []

    actual = {(d.get("kind"), d.get("metadata", {}).get("name")) for d in docs}
    for missing in sorted(EXPECTED_NAMES - actual):
        failures.append(f"expected {missing[0]} named {missing[1]}, which the chart no longer renders")
    for extra in sorted(actual - EXPECTED_NAMES):
        failures.append(f"unexpected {extra[0]} named {extra[1]}")

    deployments = [d for d in docs if d.get("kind") == "Deployment"]
    if not deployments:
        failures.append("the chart rendered no Deployment")
    for dep in deployments:
        names = [c["name"] for c in dep["spec"]["template"]["spec"]["containers"]]
        if EXPECTED_CONTAINER not in names:
            failures.append(
                f"container {EXPECTED_CONTAINER} is gone; got {names}. "
                "Dashboards select on it -- check nameOverride in values.yaml"
            )
        selector = dep["spec"]["selector"]["matchLabels"]
        if selector != EXPECTED_SELECTOR:
            failures.append(
                f"Deployment selector changed to {selector}; expected {EXPECTED_SELECTOR}. "
                "spec.selector is immutable, so this breaks upgrades on existing sites"
            )

    if failures:
        print("ERROR: the chart no longer renders the deployed identities it must preserve.")
        for f in failures:
            print(f"  - {f}")
        return 1

    print(f"  chart identities: {len(EXPECTED_NAMES)} resources, container and selector unchanged")
    return 0


if __name__ == "__main__":
    sys.exit(main())
