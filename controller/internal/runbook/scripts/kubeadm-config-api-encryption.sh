#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

set -euo pipefail

# Brownfield only: align kube-system/kubeadm-config ClusterConfiguration with NKE greenfield
# apiserver encryption settings (matches internal/kubeadmconfig when EnableEncryption is true).
#
# Prerequisites:
#   - Run on a control-plane node with kubectl access to the cluster
#   - PyYAML available for python3 (python3-yaml)

export KUBECONFIG="${KUBECONFIG:-/etc/kubernetes/admin.conf}"
CM_NAMESPACE="${CM_NAMESPACE:-kube-system}"
CM_NAME="${CM_NAME:-kubeadm-config}"
ENCRYPTION_PROVIDER_CONFIG_PATH="${ENCRYPTION_PROVIDER_CONFIG_PATH:-/etc/kubernetes/kube-api-encryption/encryption-config.yaml}"
ENCRYPTION_PROVIDER_CONFIG_DIR="${ENCRYPTION_PROVIDER_CONFIG_DIR:-/etc/kubernetes/kube-api-encryption}"
KUBEADM_CONFIG_API_ENCRYPTION_VALIDATE_ONLY="${KUBEADM_CONFIG_API_ENCRYPTION_VALIDATE_ONLY:-false}"

error() {
	echo "ERROR: $1" >&2
	exit 1
}

check_dependencies() {
	command -v kubectl >/dev/null || error "kubectl not found"
	command -v python3 >/dev/null || error "python3 not found"
	python3 -c "import yaml" >/dev/null 2>&1 || error "python3 yaml module (PyYAML) not found"
}

# Subshell so temp paths stay in scope for EXIT cleanup. A RETURN trap on a function
# runs after locals are discarded; with set -u that produced "tmpin: unbound variable".
merge_and_apply() (
	tmpin=$(mktemp)
	tmpout=$(mktemp)
	merge_py=$(mktemp)
	tmpyaml=""
	trap 'rm -f "$tmpin" "$tmpout" "$merge_py"; [[ -n "${tmpyaml:-}" ]] && rm -f "$tmpyaml"' EXIT

	kubectl get cm "$CM_NAME" -n "$CM_NAMESPACE" -o json >"$tmpin" || error "failed to read ConfigMap $CM_NAME"

	cat >"$merge_py" <<'PY'
# Exit codes: 0 = wrote updated manifest, 1 = already satisfied (no write),
#             2 = input/read/parse error, 3 = post-merge validation failed,
#             4 = unexpected error.
import copy
import json
import os
import sys
import traceback

import yaml

KEY = "ClusterConfiguration"
VOL_NAME = "enc"


def die(code, msg):
    print(msg, file=sys.stderr)
    sys.exit(code)


def load_env_paths():
    enc_path = os.environ.get("ENCRYPTION_PROVIDER_CONFIG_PATH", "").strip()
    enc_dir = os.environ.get("ENCRYPTION_PROVIDER_CONFIG_DIR", "").strip()
    if not enc_path or not enc_dir:
        die(2, "ERROR: ENCRYPTION_PROVIDER_CONFIG_PATH and ENCRYPTION_PROVIDER_CONFIG_DIR must be non-empty")
    if not enc_path.startswith("/") or not enc_dir.startswith("/"):
        die(2, "ERROR: encryption paths must be absolute file paths")
    return enc_path, enc_dir


enc_path, enc_dir = load_env_paths()

targets = {
    "encryption-provider-config": enc_path,
    "encryption-provider-config-automatic-reload": "true",
}

desired_vol = {
    "name": VOL_NAME,
    "hostPath": enc_dir,
    "mountPath": enc_dir,
    "pathType": "DirectoryOrCreate",
    "readOnly": True,
}


def merge_into(cfg):
    changed = False
    api = cfg.setdefault("apiServer", {})
    ea = api.get("extraArgs")

    if isinstance(ea, list):
        by_name = {}
        for i, item in enumerate(ea):
            if isinstance(item, dict) and "name" in item:
                by_name[item["name"]] = i
        for k, want in targets.items():
            want_s = str(want)
            if k in by_name:
                idx = by_name[k]
                cur = ea[idx].get("value")
                if cur is None or str(cur) != want_s:
                    ea[idx]["value"] = want_s
                    changed = True
            else:
                ea.append({"name": k, "value": want_s})
                changed = True
    elif isinstance(ea, dict):
        for k, want in targets.items():
            want_s = str(want)
            if ea.get(k) != want_s:
                ea[k] = want_s
                changed = True
    else:
        die(3, f"ERROR: internal: unsupported apiServer.extraArgs type {type(ea).__name__}")

    vols = api.setdefault("extraVolumes", [])
    if not isinstance(vols, list):
        api["extraVolumes"] = [copy.deepcopy(desired_vol)]
        return True

    enc_idx = None
    for i, v in enumerate(vols):
        if isinstance(v, dict) and v.get("name") == VOL_NAME:
            enc_idx = i
            break

    if enc_idx is None:
        vols.append(copy.deepcopy(desired_vol))
        changed = True
    else:
        cur = vols[enc_idx]
        need_replace = False
        for fk, fv in desired_vol.items():
            if cur.get(fk) != fv:
                need_replace = True
                break
        if need_replace:
            vols[enc_idx] = copy.deepcopy(desired_vol)
            changed = True

    return changed


def normalize_apiserver_extra_args(cfg):
    """Keep the extraArgs shape required by the stored kubeadm API version."""
    api_version = cfg.get("apiVersion")
    if api_version not in ("kubeadm.k8s.io/v1beta3", "kubeadm.k8s.io/v1beta4"):
        die(3, f"ERROR: unsupported ClusterConfiguration apiVersion {api_version!r}")

    api = cfg.setdefault("apiServer", {})
    if not isinstance(api, dict):
        die(3, "ERROR: ClusterConfiguration apiServer must be a mapping")
    ea = api.get("extraArgs")

    if api_version == "kubeadm.k8s.io/v1beta3":
        if ea is None:
            api["extraArgs"] = {}
            return
        if isinstance(ea, dict):
            api["extraArgs"] = {
                str(k): "" if v is None else str(v)
                for k, v in ea.items()
            }
            return
        if isinstance(ea, list):
            normalized = {}
            for item in ea:
                if not isinstance(item, dict) or "name" not in item:
                    die(3, f"ERROR: invalid apiServer.extraArgs entry: {item!r}")
                name = str(item["name"])
                if name in normalized:
                    die(3, f"ERROR: duplicate apiServer.extraArgs name {name!r}")
                value = item.get("value")
                normalized[name] = "" if value is None else str(value)
            api["extraArgs"] = normalized
            return
        die(3, "ERROR: apiServer.extraArgs must be a mapping for kubeadm v1beta3")

    if ea is None:
        api["extraArgs"] = []
        return
    if isinstance(ea, dict):
        api["extraArgs"] = [
            {"name": str(k), "value": "" if v is None else str(v)}
            for k, v in sorted(ea.items(), key=lambda kv: str(kv[0]))
        ]
        return
    if isinstance(ea, list):
        normalized = []
        for item in ea:
            if not isinstance(item, dict) or "name" not in item:
                die(
                    3,
                    "ERROR: apiServer.extraArgs must be a list of {name: ..., value: ...} "
                    f"mappings for kubeadm v1beta4; bad entry: {item!r}",
                )
            val = item.get("value")
            normalized.append(
                {"name": str(item["name"]), "value": "" if val is None else str(val)}
            )
        api["extraArgs"] = normalized
        return
    die(3, "ERROR: apiServer.extraArgs must be a list for kubeadm v1beta4")


def validate_merged_config(cfg):
    if not isinstance(cfg, dict):
        die(3, "ERROR: internal: ClusterConfiguration must be a mapping after merge")
    api = cfg.get("apiServer")
    if not isinstance(api, dict):
        die(3, "ERROR: merged config missing apiServer mapping")
    ea = api.get("extraArgs")
    api_version = cfg.get("apiVersion")
    if api_version == "kubeadm.k8s.io/v1beta3":
        if not isinstance(ea, dict):
            die(3, "ERROR: post-merge validation failed: extraArgs must be a mapping for kubeadm v1beta3")
        for k, value in targets.items():
            if str(ea.get(k)) != str(value):
                die(3, f"ERROR: post-merge validation failed: extraArgs {k!r} has wrong value")
    elif api_version == "kubeadm.k8s.io/v1beta4":
        if not isinstance(ea, list):
            die(3, "ERROR: post-merge validation failed: extraArgs must be a list for kubeadm v1beta4")
        names = {item.get("name") for item in ea if isinstance(item, dict)}
        for k in targets:
            if k not in names:
                die(3, f"ERROR: post-merge validation failed: extraArgs missing {k!r}")
            for item in ea:
                if isinstance(item, dict) and item.get("name") == k and str(item.get("value")) != str(targets[k]):
                    die(3, f"ERROR: post-merge validation failed: extraArgs {k!r} has wrong value")
    else:
        die(3, f"ERROR: post-merge validation failed: unsupported apiVersion {api_version!r}")

    vols = api.get("extraVolumes")
    if not isinstance(vols, list):
        die(3, "ERROR: post-merge validation failed: extraVolumes must be a list")
    found = False
    for v in vols:
        if isinstance(v, dict) and v.get("name") == VOL_NAME:
            for fk, fv in desired_vol.items():
                if v.get(fk) != fv:
                    die(3, f"ERROR: post-merge validation failed: volume {VOL_NAME!r} field {fk!r}")
            found = True
            break
    if not found:
        die(3, f"ERROR: post-merge validation failed: volume {VOL_NAME!r} not present")


def validate_yaml_roundtrip(yaml_blob):
    try:
        again = yaml.safe_load(yaml_blob)
    except yaml.YAMLError as e:
        die(3, f"ERROR: serialized ClusterConfiguration is not valid YAML: {e}")
    if not isinstance(again, dict):
        die(3, "ERROR: round-trip YAML did not produce a mapping")


def main():
    if len(sys.argv) != 3:
        die(2, "ERROR: usage: merge.py <input-configmap.json> <output-configmap.json>")

    in_path, out_path = sys.argv[1], sys.argv[2]
    try:
        with open(in_path, encoding="utf-8") as f:
            doc = json.load(f)
    except OSError as e:
        die(2, f"ERROR: cannot read input JSON: {e}")
    except json.JSONDecodeError as e:
        die(2, f"ERROR: input is not valid JSON (kubectl get output): {e}")

    if not isinstance(doc, dict):
        die(2, "ERROR: ConfigMap JSON root must be an object")

    raw = doc.get("data", {}).get(KEY)
    if raw is None or raw == "":
        die(2, f"ERROR: ConfigMap data.{KEY} is missing or empty")

    if not isinstance(raw, str):
        die(2, f"ERROR: ConfigMap data.{KEY} must be a string (YAML document), got {type(raw).__name__}")

    try:
        cfg = yaml.safe_load(raw)
    except yaml.YAMLError as e:
        die(2, f"ERROR: failed to parse existing ClusterConfiguration YAML: {e}")

    if cfg is None:
        cfg = {}

    normalize_apiserver_extra_args(cfg)
    merge_into(cfg)
    validate_merged_config(cfg)

    try:
        dumped = yaml.safe_dump(
            cfg,
            default_flow_style=False,
            sort_keys=False,
            width=4096,
        )
    except yaml.YAMLError as e:
        die(3, f"ERROR: failed to serialize ClusterConfiguration to YAML: {e}")

    validate_yaml_roundtrip(dumped)
    doc["data"][KEY] = dumped

    try:
        out_json = json.dumps(doc)
        json.loads(out_json)
    except (TypeError, ValueError) as e:
        die(3, f"ERROR: merged document is not JSON-serializable: {e}")

    try:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write(out_json)
    except OSError as e:
        die(3, f"ERROR: cannot write output JSON: {e}")

    sys.exit(0)


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception:
        traceback.print_exc(file=sys.stderr)
        sys.exit(4)
PY

	export ENCRYPTION_PROVIDER_CONFIG_PATH ENCRYPTION_PROVIDER_CONFIG_DIR
	set +e
	python3 "$merge_py" "$tmpin" "$tmpout"
	py_rc=$?
	set -e

	if [[ "$py_rc" -ne 0 ]]; then
		case "$py_rc" in
		2) error "merge script: input/parse error (code 2); see stderr above" ;;
		3) error "merge script: post-merge validation failed (code 3); see stderr above" ;;
		4) error "merge script: unexpected Python error (code 4); see stderr above" ;;
		*) error "merge script exited with code $py_rc" ;;
		esac
	fi

	if [[ ! -s "$tmpout" ]]; then
		error "merge script produced empty output file"
	fi
	python3 -c "import json,sys; json.load(open(sys.argv[1],encoding='utf-8'))" "$tmpout" || error "output file is not valid JSON (aborting kubectl apply)"

	tmpyaml=$(mktemp)
	python3 -c "
import json
import sys

path_in, path_out = sys.argv[1], sys.argv[2]
with open(path_in, encoding='utf-8') as f:
    doc = json.load(f)
raw = doc.get('data', {}).get('ClusterConfiguration')
if not isinstance(raw, str) or not raw.strip():
    print('ERROR: merged ConfigMap missing data.ClusterConfiguration string', file=sys.stderr)
    sys.exit(1)
with open(path_out, 'w', encoding='utf-8') as out:
    out.write(raw)
" "$tmpout" "$tmpyaml" || error "failed to extract ClusterConfiguration YAML for kubeadm validate"

	command -v kubeadm >/dev/null || error "kubeadm not found (required for kubeadm config validate before apply)"
	# kubeadm config validate exists from Kubernetes 1.28 onward.
	if ! kubeadm config validate --config "$tmpyaml"; then
		error "kubeadm config validate failed (refusing kubectl apply); fix ClusterConfiguration or check kubeadm / Kubernetes version"
	fi
	if [[ "$KUBEADM_CONFIG_API_ENCRYPTION_VALIDATE_ONLY" == "true" ]]; then
		echo "Validated kubeadm-config API encryption settings; no changes applied."
		return 0
	fi

	kubectl apply -f "$tmpout" || error "kubectl apply failed for $CM_NAME"
	echo "Updated kubeadm-config ConfigMap with API encryption apiServer settings."
)

main() {
	check_dependencies
	merge_and_apply
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
