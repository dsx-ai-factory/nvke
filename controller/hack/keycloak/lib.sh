#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Helpers for up.sh / down.sh. Not intended to be called by hand.

set -euo pipefail

# Cursor and some CI shells omit Homebrew. python3 and kubectl live there on macOS.
export PATH="/opt/homebrew/bin:/usr/local/bin:${PATH}"

info() { echo "[keycloak] $*"; }
die() { echo "[keycloak] ERROR: $*" >&2; exit 1; }

cmd="${1:-}"
shift || true

inject_kind_hosts() {
  local hostname_name="$1"
  local ip="$2"
  local names
  names="$(docker ps --filter "network=kind" --format '{{.Names}}')"
  [[ -n "${names}" ]] || return 0
  local name
  while IFS= read -r name; do
    [[ -n "${name}" && "${name}" != "nke-keycloak" ]] || continue
    docker exec "${name}" sh -c "
      if grep -q '${hostname_name}' /etc/hosts 2>/dev/null; then
        sed -i '/${hostname_name}/d' /etc/hosts
      fi
      echo '${ip} ${hostname_name}' >> /etc/hosts
    " >/dev/null
    info "Wrote ${ip} ${hostname_name} into ${name}:/etc/hosts"
  done <<<"${names}"
}

remove_kind_hosts() {
  local hostname_name="$1"
  local names
  names="$(docker ps --filter "network=kind" --format '{{.Names}}' 2>/dev/null || true)"
  [[ -n "${names}" ]] || return 0
  local name
  while IFS= read -r name; do
    [[ -n "${name}" && "${name}" != "nke-keycloak" ]] || continue
    docker exec "${name}" sh -c "sed -i '/${hostname_name}/d' /etc/hosts" >/dev/null 2>&1 || true
  done <<<"${names}"
}

patch_coredns() {
  local context="$1"
  local hostname_name="$2"
  local ip="$3"
  local begin="$4"
  local end="$5"
  local current
  current="$(kubectl --context "${context}" -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')"
  local patched
  patched="$(COREFILE="${current}" BEGIN="${begin}" END="${end}" HOST="${hostname_name}" IP="${ip}" python3 - <<'PY'
import os, re
core = os.environ["COREFILE"]
begin, end = os.environ["BEGIN"], os.environ["END"]
block = (
    f"    {begin}\n"
    f"    hosts {{\n"
    f"        {os.environ['IP']} {os.environ['HOST']}\n"
    f"        fallthrough\n"
    f"    }}\n"
    f"    {end}\n"
)
core = re.sub(rf"\n    {re.escape(begin)}.*?{re.escape(end)}\n", "\n", core, flags=re.S)
if ".:53 {\n" not in core:
    raise SystemExit("Corefile does not start with a .:53 server block")
core = core.replace(".:53 {\n", ".:53 {\n" + block, 1)
print(core, end="" if core.endswith("\n") else "\n")
PY
)"
  local patch
  patch="$(PATCHED="${patched}" python3 -c 'import json,os; print(json.dumps({"data":{"Corefile": os.environ["PATCHED"]}}))')"
  kubectl --context "${context}" -n kube-system patch configmap coredns --type merge -p "${patch}" >/dev/null
  kubectl --context "${context}" -n kube-system rollout restart deployment/coredns >/dev/null
  kubectl --context "${context}" -n kube-system rollout status deployment/coredns --timeout=90s >/dev/null
  info "CoreDNS in ${context} resolves ${hostname_name} to ${ip}"
}

unpatch_coredns() {
  local context="$1"
  local begin="$2"
  local end="$3"
  kubectl --context "${context}" get ns kube-system >/dev/null 2>&1 || return 0
  local current
  current="$(kubectl --context "${context}" -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}' 2>/dev/null || true)"
  [[ -n "${current}" ]] || return 0
  local patched
  patched="$(COREFILE="${current}" BEGIN="${begin}" END="${end}" python3 - <<'PY'
import os, re
core = os.environ["COREFILE"]
begin, end = os.environ["BEGIN"], os.environ["END"]
core = re.sub(rf"\n    {re.escape(begin)}.*?{re.escape(end)}\n", "\n", core, flags=re.S)
print(core, end="" if core.endswith("\n") else "\n")
PY
)"
  [[ "${patched}" != "${current}" ]] || return 0
  local patch
  patch="$(PATCHED="${patched}" python3 -c 'import json,os; print(json.dumps({"data":{"Corefile": os.environ["PATCHED"]}}))')"
  kubectl --context "${context}" -n kube-system patch configmap coredns --type merge -p "${patch}" >/dev/null
  kubectl --context "${context}" -n kube-system rollout restart deployment/coredns >/dev/null
  kubectl --context "${context}" -n kube-system rollout status deployment/coredns --timeout=90s >/dev/null || true
  info "Removed Keycloak hosts block from CoreDNS in ${context}"
}

wait_discovery() {
  local base="$1"
  local hostname_name="$2"
  local port="$3"
  local issuer="$4"
  local i
  for i in $(seq 1 60); do
    if curl -fsS --resolve "${hostname_name}:${port}:127.0.0.1" \
      "${issuer}/.well-known/openid-configuration" >/dev/null 2>&1; then
      return 0
    fi
    if curl -fsS -H "Host: ${hostname_name}:${port}" \
      "${base}/realms/nke/.well-known/openid-configuration" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  die "OIDC discovery did not become ready at ${issuer}"
}

verify_token() {
  local hostname_name="$1"
  local port="$2"
  local issuer="$3"
  local token
  token="$(curl -fsS --resolve "${hostname_name}:${port}:127.0.0.1" \
    -d "grant_type=password" \
    -d "client_id=nke-static" \
    -d "client_secret=nke-static-lab-secret" \
    -d "username=nke-dev" \
    -d "password=nke-dev" \
    -d "scope=openid" \
    "${issuer}/protocol/openid-connect/token")"
  python3 - "${token}" "${issuer}" <<'PY'
import json, sys
body = json.loads(sys.argv[1])
token = body["access_token"]
issuer = sys.argv[2]
payload = token.split(".")[1]
pad = "=" * (-len(payload) % 4)
import base64
claims = json.loads(base64.urlsafe_b64decode(payload + pad))
assert claims.get("iss") == issuer, claims.get("iss")
aud = claims.get("aud")
if isinstance(aud, str):
    aud = [aud]
assert "https://nke.tilt.local" in aud, aud
assert "nke-operators" in claims.get("groups", []), claims.get("groups")
print("[keycloak] password-grant token iss/aud/groups ok")
PY
}

verify_kind() {
  local context="$1"
  local hostname_name="$2"
  local port="$3"
  local issuer="$4"
  kubectl --context "${context}" -n kube-system delete pod nke-keycloak-probe --ignore-not-found >/dev/null
  kubectl --context "${context}" -n kube-system run nke-keycloak-probe --restart=Never --image=curlimages/curl:8.13.0 -- \
    curl -fsS --max-time 15 "${issuer}/.well-known/openid-configuration"
  kubectl --context "${context}" -n kube-system wait --for=jsonpath='{.status.phase}'=Succeeded pod/nke-keycloak-probe --timeout=60s >/dev/null
  local out
  out="$(kubectl --context "${context}" -n kube-system logs nke-keycloak-probe)"
  kubectl --context "${context}" -n kube-system delete pod nke-keycloak-probe --ignore-not-found >/dev/null
  python3 - "${out}" "${issuer}" <<'PY'
import json, sys
disc = json.loads(sys.argv[1])
issuer = sys.argv[2]
assert disc.get("issuer") == issuer, disc.get("issuer")
print("[keycloak] Kind pod reached OIDC discovery")
PY
}

case "${cmd}" in
  inject-kind-hosts) inject_kind_hosts "$@" ;;
  remove-kind-hosts) remove_kind_hosts "$@" ;;
  patch-coredns) patch_coredns "$@" ;;
  unpatch-coredns) unpatch_coredns "$@" ;;
  wait-discovery) wait_discovery "$@" ;;
  verify-token) verify_token "$@" ;;
  verify-kind) verify_kind "$@" ;;
  *) die "unknown helper ${cmd}" ;;
esac
