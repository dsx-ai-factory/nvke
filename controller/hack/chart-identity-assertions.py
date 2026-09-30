#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert that the chart still renders the deployed identities it always has,
and that a greenfield (renamed) render stays internally self-consistent.

The chart package is named `nke-controller`, but every name it renders stays
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

A second, independent render then checks the opposite direction: a site that
*does* rename (nameOverride/fullnameOverride/serviceAccount.name all set to a
greenfield value, nothing else touched) must still be internally consistent --
the ServiceMonitor's selector must still match the Service it is supposed to
scrape, and the tenant RBAC ClusterRoleBinding's subject must still match the
SPIFFE ID SPIRE actually issues that renamed ServiceAccount (see
deploy/k8s/spire-nke-services-spiffe-id.yaml's spiffeIDTemplate), with no
manual spiffe.cweSubject override required.
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
    ("CustomResourceDefinition", "clusterreleases.internal.nke.nvidia.com"),
    ("CustomResourceDefinition", "machineimages.internal.nke.nvidia.com"),
    ("CustomResourceDefinition", "nodepoolreleases.internal.nke.nvidia.com"),
    ("ClusterRole", "cloud-workflow-engine-releases"),
    ("ClusterRoleBinding", "cloud-workflow-engine-releases"),
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


GREENFIELD_NAME = "nke-controller"
GREENFIELD_TRUST_DOMAIN = "example.test"


def check_greenfield(chart: str) -> list[str]:
    """Render with a renamed service and assert it stays self-consistent."""
    failures = []

    rendered = subprocess.run(
        [
            "helm",
            "template",
            GREENFIELD_NAME,
            chart,
            "--set",
            f"nameOverride={GREENFIELD_NAME}",
            "--set",
            f"fullnameOverride={GREENFIELD_NAME}",
            "--set",
            f"serviceAccount.name={GREENFIELD_NAME}",
            "--set",
            f"spiffe.trustDomain={GREENFIELD_TRUST_DOMAIN}",
            "--set",
            "nkeOIDCAuth.enabled=true",
            "--set",
            "tenantAccess.enabled=true",
        ],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    docs = [d for d in yaml.safe_load_all(rendered) if d]

    services = [d for d in docs if d.get("kind") == "Service"]
    monitors = [d for d in docs if d.get("kind") == "ServiceMonitor"]
    if not services or not monitors:
        failures.append(
            f"greenfield render is missing a Service or ServiceMonitor: "
            f"got {len(services)} Service(s), {len(monitors)} ServiceMonitor(s)"
        )
    else:
        service_selector = services[0]["spec"]["selector"]
        monitor_selector = monitors[0]["spec"]["selector"]["matchLabels"]
        if monitor_selector != service_selector:
            failures.append(
                f"greenfield ServiceMonitor selector {monitor_selector} != Service selector {service_selector}; "
                "the ServiceMonitor would scrape nothing"
            )

    config_maps = [
        d for d in docs if d.get("kind") == "ConfigMap" and d.get("metadata", {}).get("name") == "nke-rbac"
    ]
    if not config_maps:
        failures.append("greenfield render has no nke-rbac ConfigMap")
        return failures

    inner = list(yaml.safe_load_all(config_maps[0]["data"]["manifest.yaml"]))
    bindings = [
        d for d in inner if d and d.get("kind") == "ClusterRoleBinding" and d["metadata"]["name"] == "nke:cwe"
    ]
    want_subject = f"spiffe://{GREENFIELD_TRUST_DOMAIN}/nke-service/{GREENFIELD_NAME}"
    if not bindings:
        failures.append("greenfield render has no nke:cwe ClusterRoleBinding")
    else:
        got_subjects = {s["name"] for s in bindings[0].get("subjects") or []}
        if want_subject not in got_subjects:
            failures.append(
                f"greenfield nke:cwe ClusterRoleBinding subjects {sorted(got_subjects)} "
                f"do not include {want_subject!r}; renaming the service account should not "
                "require a manual spiffe.cweSubject override"
            )

    return failures


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

    greenfield_failures = check_greenfield(args.chart)
    if greenfield_failures:
        print("ERROR: a renamed (greenfield) render is not internally self-consistent.")
        for f in greenfield_failures:
            print(f"  - {f}")
        return 1

    print("  greenfield identities: ServiceMonitor/Service selectors match, cweSubject self-corrects")
    return 0


if __name__ == "__main__":
    sys.exit(main())
