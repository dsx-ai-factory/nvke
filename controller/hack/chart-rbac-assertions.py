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


def main():
    """Render the chart and validate the embedded tenant RBAC manifest."""
    parser = argparse.ArgumentParser()
    parser.add_argument("--chart", required=True)
    parser.add_argument(
        "--tenant-rbac-name",
        default="nke:cwe",
        help="Value of nkeOIDCAuth.tenantRbacName; the rendered ClusterRole/Role/Binding objects use this name.",
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
        "nkeOIDCAuth.enabled=true",
        "--set",
        "tenantAccess.enabled=true",
        "--set",
        f"nkeOIDCAuth.tenantRbacName={tenant_rbac_name}",
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
    capi_binding = bindings.get("nke:capi-cluster-admin")
    if capi_binding is None:
        print(
            "ERROR: no nke:capi-cluster-admin ClusterRoleBinding in the nke-rbac manifest",
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
            "ERROR: nke:capi-cluster-admin subjects do not match configured identities\n"
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
            "spiffe.cweSubject=spiffe://exchange.example/cwe",
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
    print("nke:capi-cluster-admin uses all configured CAPI controller subjects")
    print("token exchange requires explicit subjects for every CAPI controller")
    return 0


if __name__ == "__main__":
    sys.exit(main())
