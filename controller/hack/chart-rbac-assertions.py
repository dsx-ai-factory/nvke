# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Assert that the rendered tenant ClusterRole grants required permissions."""

import argparse
import subprocess
import sys

import yaml


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
            and document.get("metadata", {}).get("name") == "nke-rbac"
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
        and document.get("metadata", {}).get("name") == "nke-rbac"
    ]
    if not config_maps:
        print("ERROR: no nke-rbac ConfigMap in the rendered chart", file=sys.stderr)
        return 1

    inner = list(yaml.safe_load_all(config_maps[0]["data"]["manifest.yaml"]))
    roles = {
        (document.get("kind"), document["metadata"]["name"]): document
        for document in inner
        if document and document.get("kind") in ("ClusterRole", "Role")
    }
    cluster_role = roles.get(("ClusterRole", tenant_rbac_name))
    if cluster_role is None:
        print(f"ERROR: no {tenant_rbac_name} ClusterRole in the nke-rbac manifest", file=sys.stderr)
        return 1

    bindings = {
        document["metadata"]["name"]: document
        for document in inner
        if document and document.get("kind") == "ClusterRoleBinding"
    }
    capi_binding = bindings.get("nvke:capi-cluster-admin")
    if capi_binding is None:
        print(
            "ERROR: no nvke:capi-cluster-admin ClusterRoleBinding in the nke-rbac manifest",
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
