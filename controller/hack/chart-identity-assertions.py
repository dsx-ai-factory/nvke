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
issues that renamed ServiceAccount (see deploy/k8s/spire-nvke-services-spiffe-id.yaml's
spiffeIDTemplate), with no manual spiffe.controllerSubject override required.
"""

import argparse
import dataclasses
import pathlib
import subprocess
import sys

import yaml

# The Helm release an install that predates the rename runs.
# `app.kubernetes.io/instance` and the fullname helper both derive from the
# release name, so rendering under any other one would not exercise the
# identities that actually ship.
LEGACY_RELEASE = "cloud-workflow-engine"
# The release a new install uses.
DEFAULT_RELEASE = "nvke-controller"

LEGACY_VALUES = pathlib.Path(__file__).resolve().parent / "testdata" / "chart-legacy-pinned-values.yaml"

GREENFIELD_NAME = "nke-controller"
GREENFIELD_TRUST_DOMAIN = "example.test"


def expected_names(fullname: str, config_map: str, namespace: str, rbac: str, extra: frozenset = frozenset()) -> frozenset:
    """Every (kind, name) the chart renders, given the names that vary between scenarios.

    fullname is what `controller.fullname` yields: the Deployment, Service and
    everything prefixed by it. rbac is rbacConfigMap.name, which names the
    tenant RBAC ConfigMap and suffixes the Role and RoleBinding granting get on
    it. The ServiceAccount, its Role and RoleBinding and the ServiceMonitor are
    named by their own defaults, not by fullname, and have not moved.
    """
    return (
        frozenset(
            {
                ("CustomResourceDefinition", "clusterreleases.internal.nvke.nvidia.com"),
                ("CustomResourceDefinition", "machineimages.internal.nvke.nvidia.com"),
                ("CustomResourceDefinition", "nodepoolreleases.internal.nvke.nvidia.com"),
                ("ClusterRole", f"{fullname}-releases"),
                ("ClusterRoleBinding", f"{fullname}-releases"),
                ("NetworkPolicy", f"{fullname}-metrics"),
                ("ServiceAccount", "cloud-workflow-engine"),
                ("Role", "cloud-workflow-engine"),
                ("Role", f"{fullname}-{rbac}"),
                ("RoleBinding", "cloud-workflow-engine"),
                ("RoleBinding", f"{fullname}-{rbac}"),
                ("Service", fullname),
                ("ConfigMap", config_map),
                ("Deployment", fullname),
                ("ServiceMonitor", "cloud-workflow-engine-metrics"),
                ("Pod", f"{fullname}-test-connection"),
                ("Namespace", namespace),
            }
        )
        | extra
    )


@dataclasses.dataclass(frozen=True)
class Scenario:
    """One render of the chart and what it must contain."""

    label: str
    # The Helm release name the render uses.
    release: str
    # Passed to helm as -f. None renders the chart's own defaults.
    values_file: pathlib.Path | None
    # Every (kind, name) the render must contain, and nothing else.
    names: frozenset
    # The name of the ConfigMap that carries the controller's settings file.
    config_map: str
    container: str
    selector: dict
    # The management namespace, and so the Namespace object and the namespace
    # of the RBAC ConfigMap, Role and RoleBinding.
    namespace: str
    # The teleport block of the rendered controller config.
    teleport: dict
    # The first SPIFFE path segment of the default RBAC subjects, which must be
    # the one the ClusterSPIFFEID issues.
    spiffe_segment: str = "nke-service"
    # rbacConfigMap.name: the tenant RBAC ConfigMap, and the suffix of the Role
    # and RoleBinding that grant get on it.
    rbac_name: str = "nke-rbac"


# The names an install keeps by pinning. These do not change when a default does.
LEGACY = Scenario(
    label="legacy-pinned",
    release=LEGACY_RELEASE,
    values_file=LEGACY_VALUES,
    # The pinned values set spiffe.trustDomain, so the RBAC ConfigMap renders.
    names=expected_names("cloud-workflow-engine", "nke-controller-config", "nke-system", "nke-rbac", frozenset({("ConfigMap", "nke-rbac")})),
    config_map="nke-controller-config",
    container="cloud-workflow-engine",
    selector={
        "app.kubernetes.io/name": "cloud-workflow-engine",
        "app.kubernetes.io/instance": LEGACY_RELEASE,
    },
    namespace="nke-system",
    teleport={
        "managedByLabelValue": "nke-cloud-workflow-engine",
        "tokenNamePrefix": "nke-cwe-",
        "resourceGroupLabelValue": "nke",
    },
)

# What a new install gets. The controller's own names, the config ConfigMap, the
# management namespace, the RBAC ConfigMap and the Teleport values have moved to
# nvke. The ServiceAccount and the ServiceMonitor have not, and moving them is
# the rest of the rename. Change this, and only this, when a default moves.
DEFAULT = dataclasses.replace(
    LEGACY,
    label="default",
    release=DEFAULT_RELEASE,
    values_file=None,
    names=expected_names("nvke-controller", "nvke-controller-config", "nvke-system", "nvke-rbac"),
    config_map="nvke-controller-config",
    container="nvke-controller",
    selector={
        "app.kubernetes.io/name": "nvke-controller",
        "app.kubernetes.io/instance": DEFAULT_RELEASE,
    },
    namespace="nvke-system",
    teleport={
        "managedByLabelValue": "nvke-controller",
        "tokenNamePrefix": "nvke-controller-",
        "resourceGroupLabelValue": "nvke",
    },
    spiffe_segment="nvke-service",
    rbac_name="nvke-rbac",
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


def spiffe_subjects(rbac_config_map: dict) -> set[str]:
    """Return the spiffe:// subjects of the ClusterRoleBindings the RBAC ConfigMap carries."""
    return {
        s["name"]
        for d in yaml.safe_load_all(rbac_config_map["data"]["manifest.yaml"])
        if d and d.get("kind") == "ClusterRoleBinding"
        for s in d.get("subjects") or []
        if str(s.get("name", "")).startswith("spiffe://")
    }


def check_scenario(chart: str, sc: Scenario) -> list[str]:
    """Render the scenario and assert the names, selector, namespace and config it must carry."""
    failures = []
    docs = render(chart, sc.release, sc.values_file)

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
        if not (name == sc.rbac_name or name.endswith(f"-{sc.rbac_name}")):
            continue
        got = doc["metadata"].get("namespace")
        if got != sc.namespace:
            failures.append(f"{doc['kind']} {name} is in namespace {got!r}, expected {sc.namespace!r}")

    # The RBAC ConfigMap renders only when spiffe.trustDomain is set.
    rbac = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == sc.rbac_name]
    if rbac:
        subjects = spiffe_subjects(rbac[0])
        stale = sorted(s for s in subjects if f"/{sc.spiffe_segment}/" not in s)
        if not subjects or stale:
            failures.append(
                f"SPIFFE subjects {sorted(subjects)} are not all under /{sc.spiffe_segment}/; stale: {stale}. "
                "The ClusterSPIFFEID would issue an ID the tenant bindings do not name"
            )

    config_maps = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == sc.config_map]
    if not config_maps:
        failures.append(f"no {sc.config_map} ConfigMap")
        return failures
    config = yaml.safe_load(config_maps[0]["data"]["config.yaml"])
    if config.get("managementNamespace") != sc.namespace:
        failures.append(
            f"managementNamespace is {config.get('managementNamespace')!r}, expected {sc.namespace!r}; "
            "the controller would address a namespace the chart did not grant it"
        )
    if config.get("rbacConfigMapName") != sc.rbac_name:
        failures.append(
            f"rbacConfigMapName is {config.get('rbacConfigMapName')!r}, expected {sc.rbac_name!r}; "
            "the controller would read a ConfigMap the chart did not render"
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
        d for d in docs if d.get("kind") == "ConfigMap" and d.get("metadata", {}).get("name") == DEFAULT.rbac_name
    ]
    if not config_maps:
        failures.append(f"greenfield render has no {DEFAULT.rbac_name} ConfigMap")
        return failures

    inner = list(yaml.safe_load_all(config_maps[0]["data"]["manifest.yaml"]))
    bindings = [
        d for d in inner if d and d.get("kind") == "ClusterRoleBinding" and d["metadata"]["name"] == "nvke:cwe"
    ]
    want_subject = f"spiffe://{GREENFIELD_TRUST_DOMAIN}/{DEFAULT.spiffe_segment}/{GREENFIELD_NAME}"
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
PINNED_RBAC_NAME = "pinned-rbac"

# Every ServiceAccount the RBAC manifest names a default subject for: this controller's
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
    """Render with the pinnable names set and assert they move everywhere.

    Each was a literal, so a chart that renames its defaults could not leave an
    existing site on the old names. The controller config ConfigMap name is read
    by the ConfigMap itself, the Deployment's volume and the Reloader
    annotation; the SPIFFE segment by every default subject in the RBAC
    manifest; the RBAC ConfigMap name by that ConfigMap, by the Role and
    RoleBinding granting get on it, by that Role's resourceNames, and by the
    controller through rbacConfigMapName in its config file. One consumer left
    on the default is a pod that cannot mount its config, a controller the
    tenant apiserver denies, or a Role naming a ConfigMap that no longer
    exists -- which renders clean and fails only at reconcile time.
    """
    failures = []

    docs = render(
        chart,
        DEFAULT_RELEASE,
        sets=(
            f"controllerConfig.name={PINNED_CONFIG_NAME}",
            f"spiffe.servicePathSegment={PINNED_SPIFFE_SEGMENT}",
            f"rbacConfigMap.name={PINNED_RBAC_NAME}",
            f"spiffe.trustDomain={GREENFIELD_TRUST_DOMAIN}",
            "nvkeOIDCAuth.enabled=true",
            "tenantAccess.enabled=true",
        ),
    )

    config_maps = [d["metadata"]["name"] for d in docs if d.get("kind") == "ConfigMap"]
    if PINNED_CONFIG_NAME not in config_maps or DEFAULT.config_map in config_maps:
        failures.append(f"pinned render ConfigMaps are {sorted(config_maps)}; expected {PINNED_CONFIG_NAME!r} and no {DEFAULT.config_map}")

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

    # The Role, its resourceNames and the RoleBinding all follow rbacConfigMap.name.
    # A resourceNames entry left behind is the silent failure: the ConfigMap moves,
    # the grant does not, and the controller is denied get on the manifest it needs.
    for kind in ("Role", "RoleBinding"):
        named = [d for d in docs if d.get("kind") == kind and str(d["metadata"]["name"]).endswith(PINNED_RBAC_NAME)]
        if not named:
            failures.append(f"pinned render has no {kind} suffixed {PINNED_RBAC_NAME!r}; it is still on the default name")
    for role in [d for d in docs if d.get("kind") == "Role"]:
        for rule in role.get("rules") or []:
            if "configmaps" not in (rule.get("resources") or []):
                continue
            resource_names = rule.get("resourceNames") or []
            if PINNED_RBAC_NAME not in resource_names:
                failures.append(
                    f"pinned render Role {role['metadata']['name']} grants configmaps on {sorted(resource_names)}, "
                    f"which does not include {PINNED_RBAC_NAME!r}: the ConfigMap moved and its grant did not"
                )

    config = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == PINNED_CONFIG_NAME]
    if config:
        rendered = yaml.safe_load(config[0]["data"]["config.yaml"]) or {}
        if rendered.get("rbacConfigMapName") != PINNED_RBAC_NAME:
            failures.append(
                f"pinned render controller config rbacConfigMapName is {rendered.get('rbacConfigMapName')!r}, "
                f"not {PINNED_RBAC_NAME!r}: the controller would read the wrong ConfigMap"
            )

    rbac = [d for d in docs if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == PINNED_RBAC_NAME]
    if not rbac:
        failures.append(f"pinned render has no {PINNED_RBAC_NAME} ConfigMap")
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
            f"  pinnable names: controller config ConfigMap, SPIFFE subjects and RBAC ConfigMap follow "
            f"{PINNED_CONFIG_NAME}, {PINNED_SPIFFE_SEGMENT} and {PINNED_RBAC_NAME}",
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
