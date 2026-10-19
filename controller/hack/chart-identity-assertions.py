#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert the identities the chart renders, for the three ways it gets installed.

Three renders, each with its own expectations (see SCENARIOS):

  default        the chart with no values. What a new install gets.
  legacy-pinned  the chart with hack/testdata/chart-legacy-pinned-values.yaml,
                 the values an install that predates the nvke rename sets to keep
                 the names it already runs. Every name must come out as it did
                 before the rename, whatever the defaults are.
  greenfield     a renamed install: nameOverride, fullnameOverride and
                 serviceAccount.name set to a new value and nothing else
                 touched. It must stay internally consistent.

The default render and the legacy-pinned render are separate on purpose. The
chart's defaults are the thing a rename moves; the pins are the thing that
stops that move from reaching an install that already runs. When a default
moves, DEFAULT changes and LEGACY does not, and this check fails if the change
leaks into the pinned render.

Why the legacy names are held at all: dropping or emptying `nameOverride`
renames the container, the ServiceAccount that SPIFFE identities derive from,
and the selector labels -- and `helm lint`, `helm template` and kubeconform all
still pass. Fleet dashboards select on `container="cloud-workflow-engine"`, and
a Deployment's `spec.selector` is immutable once applied, so an upgrade would
fail rather than rename.

Not asserted: `helm.sh/chart` and the `# Source:` comments. Both encode the
chart's own name by design, and neither is a selector or a consumer-visible
identity.

The greenfield render checks the opposite direction: the ServiceMonitor's
selector must still match the Service it is supposed to scrape, and the tenant
RBAC ClusterRoleBinding's subject must still match the SPIFFE ID SPIRE actually
issues that renamed ServiceAccount (see deploy/k8s/spire-nke-services-spiffe-id.yaml's
spiffeIDTemplate), with no manual spiffe.controllerSubject override required.
"""

import argparse
import dataclasses
import pathlib
import subprocess
import sys

import yaml

# The Helm release name production uses. `app.kubernetes.io/instance` and the
# fullname helper both derive from it, so rendering under any other release
# name would not exercise the identities that actually ship.
RELEASE = "cloud-workflow-engine"

LEGACY_VALUES = pathlib.Path(__file__).resolve().parent / "testdata" / "chart-legacy-pinned-values.yaml"

GREENFIELD_NAME = "nke-controller"
GREENFIELD_TRUST_DOMAIN = "example.test"

# Every resource the chart renders with no values, except the management
# Namespace, which the scenarios set.
COMMON_NAMES = frozenset(
    {
        ("CustomResourceDefinition", "clusterreleases.internal.nvke.nvidia.com"),
        ("CustomResourceDefinition", "machineimages.internal.nvke.nvidia.com"),
        ("CustomResourceDefinition", "nodepoolreleases.internal.nvke.nvidia.com"),
        ("ClusterRole", "cloud-workflow-engine-releases"),
        ("ClusterRoleBinding", "cloud-workflow-engine-releases"),
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
)


@dataclasses.dataclass(frozen=True)
class Scenario:
    """One render of the chart and what it must contain."""

    label: str
    # Passed to helm as -f. None renders the chart's own defaults.
    values_file: pathlib.Path | None
    # Every (kind, name) the render must contain, and nothing else.
    names: frozenset
    container: str
    selector: dict
    # The management namespace, and so the Namespace object and the namespace
    # of the nke-rbac ConfigMap, Role and RoleBinding.
    namespace: str
    # The teleport block of the rendered controller config.
    teleport: dict


# The names an install keeps by pinning. These do not change when a default does.
LEGACY = Scenario(
    label="legacy-pinned",
    values_file=LEGACY_VALUES,
    # The pinned values set spiffe.trustDomain, so the nke-rbac ConfigMap renders.
    names=COMMON_NAMES | {("Namespace", "nke-system"), ("ConfigMap", "nke-rbac")},
    container="cloud-workflow-engine",
    selector={
        "app.kubernetes.io/name": "cloud-workflow-engine",
        "app.kubernetes.io/instance": "cloud-workflow-engine",
    },
    namespace="nke-system",
    teleport={
        "managedByLabelValue": "nke-cloud-workflow-engine",
        "tokenNamePrefix": "nke-cwe-",
        "resourceGroupLabelValue": "nke",
    },
)

# What a new install gets. The management namespace and the Teleport values have
# moved to nvke; the object names have not, and moving them is the rest of the
# rename. Change this, and only this, when a default moves.
DEFAULT = dataclasses.replace(
    LEGACY,
    label="default",
    values_file=None,
    names=COMMON_NAMES | {("Namespace", "nvke-system")},
    namespace="nvke-system",
    teleport={
        "managedByLabelValue": "nvke-controller",
        "tokenNamePrefix": "nvke-controller-",
        "resourceGroupLabelValue": "nvke",
    },
)

SCENARIOS = (DEFAULT, LEGACY)


def render(chart: str, release: str, values_file: pathlib.Path | None = None, sets: tuple = ()) -> list[dict]:
    """Return the documents `helm template` renders."""
    cmd = ["helm", "template", release, chart]
    if values_file is not None:
        cmd += ["-f", str(values_file)]
    for item in sets:
        cmd += ["--set", item]
    out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    return [d for d in yaml.safe_load_all(out) if d]


def check_scenario(chart: str, sc: Scenario) -> list[str]:
    """Render the scenario and assert the names, selector, namespace and config it must carry."""
    failures = []
    docs = render(chart, RELEASE, sc.values_file)

    actual = {(d.get("kind"), d.get("metadata", {}).get("name")) for d in docs}
    for missing in sorted(sc.names - actual):
        failures.append(f"expected {missing[0]} named {missing[1]}, which the chart no longer renders")
    for extra in sorted(actual - sc.names):
        failures.append(f"unexpected {extra[0]} named {extra[1]}")

    deployments = [d for d in docs if d.get("kind") == "Deployment"]
    if not deployments:
        failures.append("the chart rendered no Deployment")
    for dep in deployments:
        names = [c["name"] for c in dep["spec"]["template"]["spec"]["containers"]]
        if sc.container not in names:
            failures.append(
                f"container {sc.container} is gone; got {names}. "
                "Dashboards select on it -- check nameOverride"
            )
        selector = dep["spec"]["selector"]["matchLabels"]
        if selector != sc.selector:
            failures.append(
                f"Deployment selector changed to {selector}; expected {sc.selector}. "
                "spec.selector is immutable, so this breaks upgrades on existing sites"
            )

    namespaces = [d["metadata"]["name"] for d in docs if d.get("kind") == "Namespace"]
    if namespaces != [sc.namespace]:
        failures.append(f"created Namespaces {namespaces}, expected [{sc.namespace!r}]")

    for doc in docs:
        if doc.get("kind") not in ("ConfigMap", "Role", "RoleBinding"):
            continue
        name = doc["metadata"]["name"]
        if not (name == "nke-rbac" or name.endswith("-nke-rbac")):
            continue
        got = doc["metadata"].get("namespace")
        if got != sc.namespace:
            failures.append(f"{doc['kind']} {name} is in namespace {got!r}, expected {sc.namespace!r}")

    config_maps = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "nke-controller-config"]
    if not config_maps:
        failures.append("no nke-controller-config ConfigMap")
        return failures
    config = yaml.safe_load(config_maps[0]["data"]["config.yaml"])
    if config.get("managementNamespace") != sc.namespace:
        failures.append(
            f"managementNamespace is {config.get('managementNamespace')!r}, expected {sc.namespace!r}; "
            "the controller would address a namespace the chart did not grant it"
        )
    teleport = config.get("teleport") or {}
    for key, want in sc.teleport.items():
        if teleport.get(key) != want:
            failures.append(f"teleport.{key} is {teleport.get(key)!r}, expected {want!r}")

    return failures


def check_greenfield(chart: str) -> list[str]:
    """Render with a renamed service and assert it stays self-consistent."""
    failures = []

    docs = render(
        chart,
        GREENFIELD_NAME,
        sets=(
            f"nameOverride={GREENFIELD_NAME}",
            f"fullnameOverride={GREENFIELD_NAME}",
            f"serviceAccount.name={GREENFIELD_NAME}",
            f"spiffe.trustDomain={GREENFIELD_TRUST_DOMAIN}",
            "nvkeOIDCAuth.enabled=true",
            "tenantAccess.enabled=true",
        ),
    )

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

    docs = render(
        chart,
        RELEASE,
        sets=(
            f"controllerConfig.name={PINNED_CONFIG_NAME}",
            f"spiffe.servicePathSegment={PINNED_SPIFFE_SEGMENT}",
            f"spiffe.trustDomain={GREENFIELD_TRUST_DOMAIN}",
            "nvkeOIDCAuth.enabled=true",
            "tenantAccess.enabled=true",
        ),
    )

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

    if not LEGACY_VALUES.is_file():
        print(f"ERROR: the legacy-pinned values fixture is missing: {LEGACY_VALUES}")
        return 1

    checks = [
        (
            f"the {sc.label} render",
            f"the {sc.label} render does not carry the identities it must",
            lambda sc=sc: check_scenario(args.chart, sc),
            f"  {sc.label}: {len(sc.names)} resources, container, selector, namespace and Teleport values as expected",
        )
        for sc in SCENARIOS
    ]
    checks.append(
        (
            "pinnable names",
            "pinning controllerConfig.name or spiffe.servicePathSegment does not move every consumer",
            lambda: check_pinnable_names(args.chart),
            f"  pinnable names: controller config ConfigMap and SPIFFE subjects follow {PINNED_CONFIG_NAME} and {PINNED_SPIFFE_SEGMENT}",
        )
    )
    checks.append(
        (
            "greenfield",
            "a renamed (greenfield) render is not internally self-consistent",
            lambda: check_greenfield(args.chart),
            "  greenfield: ServiceMonitor/Service selectors match, controller subject self-corrects",
        )
    )

    # Every check runs: a change to a default should fail the default render and
    # leave the legacy-pinned one passing, and stopping at the first failure
    # would hide which of the two it was.
    failed = False
    for _name, error, run, ok in checks:
        failures = run()
        if failures:
            failed = True
            print(f"ERROR: {error}.")
            for f in failures:
                print(f"  - {f}")
        else:
            print(ok)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
