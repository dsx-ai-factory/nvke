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
manual spiffe.controllerSubject override required.
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
    ("CustomResourceDefinition", "clusterreleases.internal.nvke.nvidia.com"),
    ("CustomResourceDefinition", "machineimages.internal.nvke.nvidia.com"),
    ("CustomResourceDefinition", "nodepoolreleases.internal.nvke.nvidia.com"),
    ("ClusterRole", "cloud-workflow-engine-releases"),
    ("ClusterRoleBinding", "cloud-workflow-engine-releases"),
    # The management namespace is the one default that moved: a new install gets
    # nvke-system. An existing site pins nvkeSystemNamespace.name to nke-system,
    # which check_legacy_namespace renders and asserts.
    ("Namespace", "nvke-system"),
    ("NetworkPolicy", "cloud-workflow-engine-metrics"),
    ("ServiceAccount", "cloud-workflow-engine"),
    ("Role", "cloud-workflow-engine"),
    ("Role", "cloud-workflow-engine-nke-rbac"),
    ("RoleBinding", "cloud-workflow-engine"),
    ("RoleBinding", "cloud-workflow-engine-nke-rbac"),
    ("Service", "cloud-workflow-engine"),
    ("ConfigMap", "nke-controller-config"),
    ("Deployment", "cloud-workflow-engine"),
    ("ServiceMonitor", "cloud-workflow-engine-metrics"),
    ("Pod", "cloud-workflow-engine-test-connection"),
}

EXPECTED_CONTAINER = "cloud-workflow-engine"
EXPECTED_SELECTOR = {
    "app.kubernetes.io/name": "cloud-workflow-engine",
    "app.kubernetes.io/instance": "cloud-workflow-engine",
}


LEGACY_NAMESPACE = "nke-system"

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
            "nvkeOIDCAuth.enabled=true",
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
        d for d in inner if d and d.get("kind") == "ClusterRoleBinding" and d["metadata"]["name"] == "nvke:cwe"
    ]
    want_subject = f"spiffe://{GREENFIELD_TRUST_DOMAIN}/nke-service/{GREENFIELD_NAME}"
    if not bindings:
        failures.append("greenfield render has no nvke:cwe ClusterRoleBinding")
    else:
        got_subjects = {s["name"] for s in bindings[0].get("subjects") or []}
        if want_subject not in got_subjects:
            failures.append(
                f"greenfield nvke:cwe ClusterRoleBinding subjects {sorted(got_subjects)} "
                f"do not include {want_subject!r}; renaming the service account should not "
                "require a manual spiffe.controllerSubject override"
            )

    return failures


def check_legacy_namespace(chart: str) -> list[str]:
    """Render with the management namespace pinned and assert everything moves together.

    A site installed before nvkeSystemNamespace.name existed holds its
    tenant-access Secrets in nke-system, and the old namespace is not also read.
    Pinning the value is how such a site stays put, so the pin is gated here:
    the Namespace, the nke-rbac ConfigMap, the Role and RoleBinding scoped to it
    and the managementNamespace the controller reads must all land on the pinned
    name. One of them left on the default is a site whose controller publishes
    where it has no grant, or reads where nothing is published.
    """
    failures = []

    rendered = subprocess.run(
        [
            "helm",
            "template",
            RELEASE,
            chart,
            "--set",
            f"nvkeSystemNamespace.name={LEGACY_NAMESPACE}",
            "--set",
            f"spiffe.trustDomain={GREENFIELD_TRUST_DOMAIN}",
        ],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    docs = [d for d in yaml.safe_load_all(rendered) if d]

    namespaces = [d for d in docs if d.get("kind") == "Namespace"]
    if [d["metadata"]["name"] for d in namespaces] != [LEGACY_NAMESPACE]:
        failures.append(
            f"pinned render created {[d['metadata']['name'] for d in namespaces]}, expected [{LEGACY_NAMESPACE!r}]"
        )

    for kind in ("ConfigMap", "Role", "RoleBinding"):
        for doc in docs:
            if doc.get("kind") != kind:
                continue
            name = doc["metadata"]["name"]
            if not (name == "nke-rbac" or name.endswith("-nke-rbac")):
                continue
            got = doc["metadata"].get("namespace")
            if got != LEGACY_NAMESPACE:
                failures.append(f"pinned render put {kind} {name} in {got!r}, expected {LEGACY_NAMESPACE!r}")

    config = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "nke-controller-config"]
    if not config:
        failures.append("pinned render has no nke-controller-config ConfigMap")
    else:
        got = yaml.safe_load(config[0]["data"]["config.yaml"]).get("managementNamespace")
        if got != LEGACY_NAMESPACE:
            failures.append(
                f"pinned render set managementNamespace to {got!r}, expected {LEGACY_NAMESPACE!r}; "
                "the controller would address a namespace the chart did not grant it"
            )

    return failures


PINNED_CONFIG_NAME = "pinned-controller-config"
PINNED_SPIFFE_SEGMENT = "pinned-service"

# Every ServiceAccount nke-rbac names a default subject for: this controller's
# own, plus the four CAPI managers the nvke:capi-cluster-admin binding grants.
# Each is spiffe://<trustDomain>/<servicePathSegment>/<serviceAccount>, so
# pinning the segment has to move all five together.
DEFAULT_SPIFFE_SERVICE_ACCOUNTS = (
    "cloud-workflow-engine",
    "capi-manager",
    "capi-kubeadm-control-plane-manager",
    "capi-kubeadm-bootstrap-manager",
    "capd-manager",
)


def check_pinnable_names(chart: str) -> list[str]:
    """Render with controllerConfig.name and spiffe.servicePathSegment set and assert they move everywhere.

    Both were literals, so a chart that renames its defaults could not leave an
    existing site on the old names. The ConfigMap name is read by the ConfigMap
    itself, the Deployment's volume and the Reloader annotation; the SPIFFE
    segment by every default subject in nke-rbac. One consumer left on the
    default is a pod that cannot mount its config or a controller the tenant
    apiserver denies.
    """
    failures = []

    rendered = subprocess.run(
        [
            "helm",
            "template",
            RELEASE,
            chart,
            "--set",
            f"controllerConfig.name={PINNED_CONFIG_NAME}",
            "--set",
            f"spiffe.servicePathSegment={PINNED_SPIFFE_SEGMENT}",
            "--set",
            f"spiffe.trustDomain={GREENFIELD_TRUST_DOMAIN}",
            "--set",
            "nvkeOIDCAuth.enabled=true",
            "--set",
            "tenantAccess.enabled=true",
        ],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    docs = [d for d in yaml.safe_load_all(rendered) if d]

    config_maps = [d["metadata"]["name"] for d in docs if d.get("kind") == "ConfigMap"]
    if PINNED_CONFIG_NAME not in config_maps or "nke-controller-config" in config_maps:
        failures.append(f"pinned render ConfigMaps are {sorted(config_maps)}; expected {PINNED_CONFIG_NAME!r} and no nke-controller-config")

    deployments = [d for d in docs if d.get("kind") == "Deployment"]
    if not deployments:
        failures.append(
            "pinned render has no Deployment; the config mount and Reloader watch went unchecked"
        )
    for dep in deployments:
        volumes = {v["name"]: v for v in dep["spec"]["template"]["spec"].get("volumes") or []}
        mounted = [v["configMap"]["name"] for v in volumes.values() if "configMap" in v]
        if PINNED_CONFIG_NAME not in mounted:
            failures.append(f"pinned render Deployment mounts ConfigMaps {mounted}, not {PINNED_CONFIG_NAME!r}")
        reload = dep["metadata"].get("annotations", {}).get("configmap.reloader.stakater.com/reload", "")
        if PINNED_CONFIG_NAME not in reload.split(","):
            failures.append(f"pinned render Reloader annotation is {reload!r}, which does not watch {PINNED_CONFIG_NAME!r}")

    rbac = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "nke-rbac"]
    if not rbac:
        failures.append("pinned render has no nke-rbac ConfigMap")
        return failures
    subjects = {
        str(s.get("name", ""))
        for d in yaml.safe_load_all(rbac[0]["data"]["manifest.yaml"])
        if d and d.get("kind") == "ClusterRoleBinding"
        for s in d.get("subjects") or []
    }
    want = {
        f"spiffe://{GREENFIELD_TRUST_DOMAIN}/{PINNED_SPIFFE_SEGMENT}/{sa}" for sa in DEFAULT_SPIFFE_SERVICE_ACCOUNTS
    }
    if subjects != want:
        failures.append(
            f"pinned render ClusterRoleBinding subjects {sorted(subjects)} != expected {sorted(want)}; "
            f"missing: {sorted(want - subjects)}; unexpected: {sorted(subjects - want)}"
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

    legacy_failures = check_legacy_namespace(args.chart)
    if legacy_failures:
        print("ERROR: pinning nvkeSystemNamespace.name does not keep an existing site in its namespace.")
        for f in legacy_failures:
            print(f"  - {f}")
        return 1

    print(f"  pinned management namespace: every object and the controller config land in {LEGACY_NAMESPACE}")

    pinnable_failures = check_pinnable_names(args.chart)
    if pinnable_failures:
        print("ERROR: pinning controllerConfig.name or spiffe.servicePathSegment does not move every consumer.")
        for f in pinnable_failures:
            print(f"  - {f}")
        return 1

    print(f"  pinnable names: controller config ConfigMap and SPIFFE subjects follow {PINNED_CONFIG_NAME} and {PINNED_SPIFFE_SEGMENT}")

    greenfield_failures = check_greenfield(args.chart)
    if greenfield_failures:
        print("ERROR: a renamed (greenfield) render is not internally self-consistent.")
        for f in greenfield_failures:
            print(f"  - {f}")
        return 1

    print("  greenfield identities: ServiceMonitor/Service selectors match, controller subject self-corrects")
    return 0


if __name__ == "__main__":
    sys.exit(main())
