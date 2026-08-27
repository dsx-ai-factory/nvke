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
    args = parser.parse_args()

    command = [
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
    cluster_role = roles.get(("ClusterRole", "nke:cwe"))
    if cluster_role is None:
        print("ERROR: no nke:cwe ClusterRole in the nke-rbac manifest", file=sys.stderr)
        return 1

    # API group, resource, verb, optional resource name, and the caller that
    # breaks without it. Keep the signer permission restricted to
    # kubelet-serving rather than accepting approval of every signer.
    required = [
        ("", "nodes", "patch", None, "node updates"),
        ("", "nodes", "delete", None, "node teardown"),
        ("", "nodes/status", "patch", None, "ApplyNodeCondition"),
        ("", "pods/eviction", "create", None, "node drain"),
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
            print(f"ERROR: nke:cwe ClusterRole is missing {permission}", file=sys.stderr)
        return 1

    print(f"nke:cwe ClusterRole grants all {len(required)} required permissions")
    return 0


if __name__ == "__main__":
    sys.exit(main())
