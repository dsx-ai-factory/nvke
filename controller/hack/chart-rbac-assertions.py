# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert that the rendered tenant ClusterRole grants required permissions."""

import argparse
import copy
import subprocess
import sys

import yaml

# The ConfigMap carrying the tenant RBAC manifest. Its name is a chart value
# (rbacConfigMap.name), so this script sets it rather than reading whatever the
# default happens to be: these assertions are about the permissions the manifest
# grants, and moving the default must not break them. That the value drives the
# ConfigMap, its Role, that Role's resourceNames and the RoleBinding is
# chart-identity-assertions.py's check, not this one's.
RBAC_CONFIG_MAP = "nvke-rbac"


def granted(rule_set, api_group, resource, verb, resource_name=None):
    """Return whether a rule grants the requested permission."""
    for rule in rule_set:
        if api_group not in (rule.get("apiGroups") or []):
            continue
        if resource not in (rule.get("resources") or []):
            continue
        if verb not in (rule.get("verbs") or []):
            continue
        if resource_name is not None and rule.get("resourceNames") != [resource_name]:
            continue
        return True
    return False


def controller_subjects(rendered, tenant_rbac_name):
    """Return each tenant RBAC binding that names the controller, with its subjects."""
    for document in yaml.safe_load_all(rendered):
        if (
            document
            and document.get("kind") == "ConfigMap"
            and document.get("metadata", {}).get("name") == RBAC_CONFIG_MAP
        ):
            inner = yaml.safe_load_all(document["data"]["manifest.yaml"])
            return {
                (binding["kind"], binding["metadata"].get("namespace", "")): [
                    subject.get("name") for subject in binding.get("subjects") or []
                ]
                for binding in inner
                if binding
                and binding.get("kind") in ("ClusterRoleBinding", "RoleBinding")
                and binding["metadata"]["name"] == tenant_rbac_name
            }
    return {}


def main():
    """Render the chart and validate the embedded tenant RBAC manifest."""
    parser = argparse.ArgumentParser()
    parser.add_argument("--chart", required=True)
    parser.add_argument(
        "--tenant-rbac-name",
        default="nvke:cwe",
        help="Value of nvkeOIDCAuth.tenantRbacName; the rendered ClusterRole/Role/Binding objects use this name.",
    )
    args = parser.parse_args()
    tenant_rbac_name = args.tenant_rbac_name

    base_command = [
        "helm",
        "template",
        "cwe",
        args.chart,
        "--set",
        "spiffe.trustDomain=example.test",
        "--set",
        "nvkeOIDCAuth.enabled=true",
        "--set",
        "tenantAccess.enabled=true",
        "--set",
        f"nvkeOIDCAuth.tenantRbacName={tenant_rbac_name}",
        "--set",
        f"rbacConfigMap.name={RBAC_CONFIG_MAP}",
    ]
    command = [
        *base_command,
        "--set",
        "spiffe.capiManagerSubject=spiffe://exchange.example/capi-manager",
        "--set",
        "spiffe.capiKubeadmControlPlaneManagerSubject=spiffe://exchange.example/kcp-manager",
        "--set",
        "spiffe.capiKubeadmBootstrapManagerSubject=spiffe://exchange.example/kubeadm-bootstrap-manager",
        "--set",
        "spiffe.capdManagerSubject=spiffe://exchange.example/capd-manager",
    ]
    rendered = subprocess.run(command, check=False, capture_output=True, text=True)
    if rendered.returncode != 0:
        sys.stderr.write(rendered.stderr)
        return rendered.returncode

    # The RBAC sits inside a ConfigMap value, so it is parsed twice.
    documents = [document for document in yaml.safe_load_all(rendered.stdout) if document]
    config_maps = [
        document
        for document in documents
        if document.get("kind") == "ConfigMap"
        and document.get("metadata", {}).get("name") == RBAC_CONFIG_MAP
    ]
    if not config_maps:
        print(f"ERROR: no {RBAC_CONFIG_MAP} ConfigMap in the rendered chart", file=sys.stderr)
        return 1

    inner = list(yaml.safe_load_all(config_maps[0]["data"]["manifest.yaml"]))
    additional_rbac = [
        {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRole",
            "metadata": {"name": "node-reader"},
            "rules": [{"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list"]}],
        },
        {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRoleBinding",
            "metadata": {"name": "node-reader"},
            "subjects": [{"kind": "User", "name": "spiffe://example.test/node-reader", "apiGroup": "rbac.authorization.k8s.io"}],
            "roleRef": {"kind": "ClusterRole", "name": "node-reader", "apiGroup": "rbac.authorization.k8s.io"},
        },
    ]
    scoped_rbac = []
    for namespace in ("tenant-a", "tenant-b"):
        for kind in ("Role", "RoleBinding"):
            obj = copy.deepcopy(next(document for document in inner if document["kind"] == kind))
            obj["metadata"]["namespace"] = namespace
            if kind == "RoleBinding":
                obj["metadata"]["annotations"] = {"nvke.nvidia.com/identity-source": "operator"}
            scoped_rbac.append(obj)
    for additional in ([], None, additional_rbac, scoped_rbac):
        result = subprocess.run(
            [*command, "--values", "-"],
            input=yaml.safe_dump({"tenantAccess": {"additionalRBAC": additional}}),
            check=False,
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            sys.stderr.write(result.stderr)
            return result.returncode
        actual_documents = [document for document in yaml.safe_load_all(result.stdout) if document]
        actual_config_map = next(
            document for document in actual_documents
            if document.get("kind") == "ConfigMap" and document["metadata"]["name"] == RBAC_CONFIG_MAP
        )
        actual_rbac = list(yaml.safe_load_all(actual_config_map["data"]["manifest.yaml"]))
        if actual_rbac != inner + (additional or []):
            print("ERROR: additional tenant RBAC must append complete documents without changing existing RBAC", file=sys.stderr)
            return 1
        actual_config_map["data"]["manifest.yaml"] = config_maps[0]["data"]["manifest.yaml"]
        if actual_documents != documents:
            print(f"ERROR: additional tenant RBAC must only change the {RBAC_CONFIG_MAP} manifest", file=sys.stderr)
            return 1

    for override, expected in (
        ("spiffe.trustDomain=", "spiffe.trustDomain must be set when tenantAccess.additionalRBAC is non-empty"),
        ("tenantAccess.enabled=false", "tenantAccess.enabled must be true when tenantAccess.additionalRBAC is non-empty"),
    ):
        for additional in ([], None, additional_rbac):
            result = subprocess.run(
                [*command, "--set", "nvkeOIDCAuth.enabled=false", "--set", override, "--values", "-"],
                input=yaml.safe_dump({"tenantAccess": {"additionalRBAC": additional}}),
                check=False,
                capture_output=True,
                text=True,
            )
            if additional:
                if result.returncode == 0 or expected not in result.stderr:
                    print(f"ERROR: expected render rejection: {expected}\n{result.stderr}", file=sys.stderr)
                    return 1
            elif result.returncode != 0:
                print(f"ERROR: empty additional RBAC must allow {override}\n{result.stderr}", file=sys.stderr)
                return 1

    for api_version, kind, invalid_field in (
        ("v1", "Secret", "kind"),
        ("v1", "ConfigMap", "kind"),
        ("rbac.authorization.k8s.io/v1", None, "kind"),
        ("example.test/v1", "Role", "apiVersion"),
        ("rbac.authorization.k8s.io/v1beta1", "ClusterRole", "apiVersion"),
        (None, "RoleBinding", "apiVersion"),
    ):
        obj = {"metadata": {"name": "invalid-object", "namespace": "tenant-a"}}
        if api_version is not None:
            obj["apiVersion"] = api_version
        if kind is not None:
            obj["kind"] = kind
        if kind == "Secret":
            obj["stringData"] = {"marker": "not-a-credential"}
        result = subprocess.run(
            [*command, "--values", "-"],
            input=yaml.safe_dump({"tenantAccess": {"additionalRBAC": [obj]}}),
            check=False,
            capture_output=True,
            text=True,
        )
        expected = f"tenantAccess.additionalRBAC[0]: {invalid_field} must be"
        if result.returncode == 0 or expected not in result.stderr:
            print(f"ERROR: expected render rejection: {expected}\n{result.stderr}", file=sys.stderr)
            return 1

    invalid_cases = [[document] for document in inner]
    for obj in additional_rbac + scoped_rbac:
        invalid_cases.append([obj, obj])
    for document in inner + additional_rbac:
        if document["kind"] in ("ClusterRole", "ClusterRoleBinding"):
            obj = copy.deepcopy(document)
            obj["metadata"]["namespace"] = "invalid-for-cluster-scoped-rbac"
            invalid_cases.append([obj])
    for kind in ("Role", "RoleBinding"):
        for namespace in (None, ""):
            obj = copy.deepcopy(next(document for document in scoped_rbac if document["kind"] == kind))
            if namespace is None:
                obj["metadata"].pop("namespace")
            else:
                obj["metadata"]["namespace"] = namespace
            invalid_cases.append([obj])
    for additional in invalid_cases:
        result = subprocess.run(
            [*command, "--values", "-"],
            input=yaml.safe_dump({"tenantAccess": {"additionalRBAC": additional}}),
            check=False,
            capture_output=True,
            text=True,
        )
        obj = additional[-1]
        namespace = obj["metadata"].get("namespace", "") if obj["kind"] in ("Role", "RoleBinding") else ""
        identity = f"{obj['kind']}/{namespace}/{obj['metadata']['name']}"
        expected = f"tenantAccess.additionalRBAC[{len(additional) - 1}]: duplicate RBAC object {identity}"
        if obj["kind"] in ("ClusterRole", "ClusterRoleBinding") and obj["metadata"].get("namespace"):
            expected = f"tenantAccess.additionalRBAC[0]: {obj['kind']} must not set metadata.namespace"
        elif obj["kind"] in ("Role", "RoleBinding") and not obj["metadata"].get("namespace"):
            expected = f"tenantAccess.additionalRBAC[0]: {obj['kind']} must set metadata.namespace"
        if result.returncode == 0 or expected not in result.stderr:
            print(f"ERROR: expected render rejection: {expected}\n{result.stderr}", file=sys.stderr)
            return 1

    roles = {
        (document.get("kind"), document["metadata"]["name"]): document
        for document in inner
        if document and document.get("kind") in ("ClusterRole", "Role")
    }
    cluster_role = roles.get(("ClusterRole", tenant_rbac_name))
    if cluster_role is None:
        print(f"ERROR: no {tenant_rbac_name} ClusterRole in the {RBAC_CONFIG_MAP} manifest", file=sys.stderr)
        return 1

    bindings = {
        document["metadata"]["name"]: document
        for document in inner
        if document and document.get("kind") == "ClusterRoleBinding"
    }
    capi_binding = bindings.get("nvke:capi-cluster-admin")
    if capi_binding is None:
        print(
            f"ERROR: no nvke:capi-cluster-admin ClusterRoleBinding in the {RBAC_CONFIG_MAP} manifest",
            file=sys.stderr,
        )
        return 1

    capi_subjects = {
        subject.get("name") for subject in capi_binding.get("subjects") or []
    }
    expected_capi_subjects = {
        "spiffe://exchange.example/capi-manager",
        "spiffe://exchange.example/kcp-manager",
        "spiffe://exchange.example/kubeadm-bootstrap-manager",
        "spiffe://exchange.example/capd-manager",
    }
    if capi_subjects != expected_capi_subjects:
        print(
            "ERROR: nvke:capi-cluster-admin subjects do not match configured identities\n"
            f"  got:  {sorted(capi_subjects)}\n"
            f"  want: {sorted(expected_capi_subjects)}",
            file=sys.stderr,
        )
        return 1

    exchange_subjects = {
        "spiffe.capiManagerSubject": "spiffe://exchange.example/capi-manager",
        "spiffe.capiKubeadmControlPlaneManagerSubject": "spiffe://exchange.example/kcp-manager",
        "spiffe.capiKubeadmBootstrapManagerSubject": "spiffe://exchange.example/kubeadm-bootstrap-manager",
        "spiffe.capdManagerSubject": "spiffe://exchange.example/capd-manager",
    }
    for missing_name in exchange_subjects:
        missing_subject_command = [
            *base_command,
            "--set",
            "spiffe.jwtExchangeEndpoint=https://exchange.example/v1/exchange",
            "--set",
            "spiffe.controllerSubject=spiffe://exchange.example/controller",
        ]
        for name, value in exchange_subjects.items():
            if name != missing_name:
                missing_subject_command.extend(["--set", f"{name}={value}"])
        missing_subject = subprocess.run(
            missing_subject_command,
            check=False,
            capture_output=True,
            text=True,
        )
        expected_error = f"{missing_name} must be set"
        if missing_subject.returncode == 0 or expected_error not in missing_subject.stderr:
            print(
                f"ERROR: token exchange without {missing_name} must fail with: {expected_error}",
                file=sys.stderr,
            )
            return 1

    # spiffe.cweSubject is the older name for spiffe.controllerSubject. Either
    # one satisfies the exchange requirement, and the new one wins when both are set.
    exchange_command = [*base_command, "--set", "spiffe.jwtExchangeEndpoint=https://exchange.example/v1/exchange"]
    for name, value in exchange_subjects.items():
        exchange_command.extend(["--set", f"{name}={value}"])
    controller_subject_cases = [
        ("neither key", [], None),
        ("only spiffe.cweSubject", ["spiffe.cweSubject=spiffe://exchange.example/old"], "spiffe://exchange.example/old"),
        ("only spiffe.controllerSubject", ["spiffe.controllerSubject=spiffe://exchange.example/new"], "spiffe://exchange.example/new"),
        (
            "an empty spiffe.controllerSubject",
            ["spiffe.controllerSubject=", "spiffe.cweSubject=spiffe://exchange.example/old"],
            "spiffe://exchange.example/old",
        ),
        (
            "both keys",
            ["spiffe.cweSubject=spiffe://exchange.example/old", "spiffe.controllerSubject=spiffe://exchange.example/new"],
            "spiffe://exchange.example/new",
        ),
    ]
    for case, settings, want in controller_subject_cases:
        case_command = list(exchange_command)
        for setting in settings:
            case_command.extend(["--set", setting])
        result = subprocess.run(case_command, check=False, capture_output=True, text=True)
        if want is None:
            expected_error = "spiffe.controllerSubject (or the older spiffe.cweSubject) must be set"
            if result.returncode == 0 or expected_error not in result.stderr:
                print(
                    f"ERROR: token exchange with {case} must fail with: {expected_error}",
                    file=sys.stderr,
                )
                return 1
            continue
        if result.returncode != 0:
            sys.stderr.write(result.stderr)
            return result.returncode
        got = controller_subjects(result.stdout, tenant_rbac_name)
        expected = {
            ("ClusterRoleBinding", ""): [want],
            ("RoleBinding", "kube-system"): [want],
            ("RoleBinding", "kube-node-lease"): [want],
        }
        if got != expected:
            print(
                f"ERROR: token exchange with {case}: {tenant_rbac_name} bindings are {got}, want {expected}",
                file=sys.stderr,
            )
            return 1

    # A site that moves its value from the old key to the new one must render
    # exactly what it rendered before.
    same_value_renders = [
        subprocess.run(
            [*exchange_command, "--set", f"{key}=spiffe://exchange.example/controller"],
            check=False,
            capture_output=True,
            text=True,
        )
        for key in ("spiffe.cweSubject", "spiffe.controllerSubject")
    ]
    for result in same_value_renders:
        if result.returncode != 0:
            sys.stderr.write(result.stderr)
            return result.returncode
    if same_value_renders[0].stdout != same_value_renders[1].stdout:
        print(
            "ERROR: the same subject set through spiffe.cweSubject and through "
            "spiffe.controllerSubject renders different charts",
            file=sys.stderr,
        )
        return 1

    # API group, resource, verb, optional resource name, and the caller that
    # breaks without it. Keep the signer permission restricted to
    # kubelet-serving rather than accepting approval of every signer.
    required = [
        ("", "nodes", "patch", None, "node updates"),
        ("", "nodes", "delete", None, "node teardown"),
        ("", "nodes/status", "patch", None, "ApplyNodeCondition"),
        ("", "pods/eviction", "create", None, "node drain"),
        ("", "namespaces", "get", "kube-system", "LabelSystemNamespacesForPSA"),
        ("", "namespaces", "patch", "kube-system", "LabelSystemNamespacesForPSA"),
        ("", "namespaces", "get", "tigera-operator", "LabelSystemNamespacesForPSA"),
        ("", "namespaces", "patch", "tigera-operator", "LabelSystemNamespacesForPSA"),
        ("", "namespaces", "get", "ovn-kubernetes", "LabelSystemNamespacesForPSA"),
        ("", "namespaces", "patch", "ovn-kubernetes", "LabelSystemNamespacesForPSA"),
        (
            "certificates.k8s.io",
            "certificatesigningrequests",
            "get",
            None,
            "CSR reconciliation",
        ),
        (
            "certificates.k8s.io",
            "certificatesigningrequests",
            "list",
            None,
            "CSR controller cache",
        ),
        (
            "certificates.k8s.io",
            "certificatesigningrequests",
            "watch",
            None,
            "CSR controller watch",
        ),
        (
            "certificates.k8s.io",
            "certificatesigningrequests/approval",
            "update",
            None,
            "CSR approval update",
        ),
        (
            "certificates.k8s.io",
            "signers",
            "approve",
            "kubernetes.io/kubelet-serving",
            "kubelet-serving approval",
        ),
    ]
    missing = [
        f"{group or '<core>'}/{resource} ({verb}) - needed by {reason}"
        for group, resource, verb, resource_name, reason in required
        if not granted(
            cluster_role.get("rules") or [],
            group,
            resource,
            verb,
            resource_name,
        )
    ]
    if missing:
        for permission in missing:
            print(f"ERROR: {tenant_rbac_name} ClusterRole is missing {permission}", file=sys.stderr)
        return 1

    print(f"{tenant_rbac_name} ClusterRole grants all {len(required)} required permissions")
    print("nvke:capi-cluster-admin uses all configured CAPI controller subjects")
    print("token exchange requires explicit subjects for every CAPI controller")
    print(f"{tenant_rbac_name} bindings take spiffe.controllerSubject over spiffe.cweSubject, render the same for either, and need one with token exchange")
    return 0


if __name__ == "__main__":
    sys.exit(main())
