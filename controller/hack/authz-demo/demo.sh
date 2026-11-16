#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The authorization demo's scripted walkthrough. See usage() and README.md.
#
# show evals its argument, so single-quoted commands expand variables when they run.
# shellcheck disable=SC2016,SC2153

set -uo pipefail

usage() {
  cat <<'EOF'
demo.sh [--setup] [--with-teleport]

  --setup          return the demo to its starting state and print "ready"
  --with-teleport  include Teleport: in --setup, its users and roles; in the
                   walkthrough, section 8

Runs against kind-controller-capi with NVKE_AUTHZ_DEMO=true tilt up. Prints one
PASS or FAIL line per check. Exits 0 when every check passes, 1 when one fails,
and 2 when it can't run, such as when the controller isn't ready.
EOF
}

CTX=kind-controller-capi
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROLLER_DIR="$(cd "${HERE}/../.." && pwd)"
OUT="${CONTROLLER_DIR}/.local/authz-demo"

SETUP=0
TELEPORT=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --setup) SETUP=1 ;;
    --with-teleport) TELEPORT=1 ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done

# The variables the on-screen commands use.
API=http://localhost:8080/api
KC=https://localhost:18443/realms/nvke
TP=https://nvke.teleport.nvke-demo.local:3080
export API KC TP CTX

FAILED=0
ROGUE_ISS=https://rogue.nvke-demo.local

die() { echo "demo.sh: $*" >&2; exit 2; }

# show prints a command and runs it in this shell.
show() {
  printf '\n$ %s\n' "$1"
  eval "$1"
}

check() {
  local n="$1" text="$2"
  shift 2
  if "$@"; then
    echo "PASS ${n} ${text}"
  else
    echo "FAIL ${n} ${text}"
    FAILED=1
  fi
}

# rfc3339_ago SECONDS: the UTC time SECONDS ago. The kind node's clock can lag
# the laptop's, so log windows start a minute early. Each log check also
# filters on the caller and the action.
rfc3339_ago() {
  local t=$(( $(date +%s) - $1 ))
  date -u -r "${t}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@${t}" +%Y-%m-%dT%H:%M:%SZ
}

# wait_for TIMEOUT CMD...: retry CMD quietly every second.
wait_for() {
  local timeout="$1" start=${SECONDS}
  shift
  until "$@" >/dev/null 2>&1; do
    (( SECONDS - start >= timeout )) && return 1
    sleep 1
  done
}

kc_jwks() { curl -fsS --cacert "${OUT}/ca.crt" "${KC}/protocol/openid-connect/certs"; }
tp_jwks() { curl -fsS --cacert "${OUT}/ca.crt" https://localhost:3080/.well-known/jwks.json; }
teleport() { kubectl --context "${CTX}" -n nvke-authz-demo exec deploy/teleport -- "$@"; }
controller_config() {
  kubectl --context "${CTX}" -n nvke-controller get configmap nvke-controller-config -o jsonpath='{.data.config\.yaml}'
}
password_token() {
  curl -fsS --cacert "${OUT}/ca.crt" -d grant_type=password -d client_id=nvkectl \
    -d username="$1" -d password="$1-demo" "${KC}/protocol/openid-connect/token" | jq -er .access_token
}
alice_lists_clusters() {
  local token
  token="$(password_token alice)" || return 1
  [[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${token}" "${API}/clusters")" == 200 ]]
}
build_mint() {
  (cd "${CONTROLLER_DIR}" && go build -o "${OUT}/mint" ./hack/authz-demo/mint) || die "building the minter failed"
}
teleport_app_registered() { teleport tctl apps ls --format=json | grep -q '"name": "nvke"'; }

preflight() {
  local tool
  for tool in curl jq yq go kubectl uuidgen; do
    command -v "${tool}" >/dev/null || die "${tool} is not installed"
  done
  kubectl config get-contexts "${CTX}" >/dev/null 2>&1 || die "no kube context ${CTX}"
  [[ -f "${OUT}/ca.crt" ]] || die "${OUT}/ca.crt is missing; run hack/authz-demo/gen.sh"
  [[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/readyz)" == 200 ]] ||
    die "the controller is not ready on localhost:8080; run NVKE_AUTHZ_DEMO=true tilt up"
  kc_jwks >/dev/null || die "Keycloak's JWKS is not reachable at ${KC}; is the Tilt port-forward up?"
  [[ "$(controller_config | yq '.apiAuthz.enabled')" == true ]] ||
    die "nvke-controller-config does not set apiAuthz.enabled: true; run NVKE_AUTHZ_DEMO=true tilt up"
  if (( TELEPORT )); then
    tp_jwks >/dev/null || die "Teleport's JWKS is not reachable on localhost:3080; run NVKE_AUTHZ_DEMO_TELEPORT=true tilt up"
    [[ "$(controller_config | yq '.apiAuth.issuers[] | select(.name == "teleport") | .name')" == teleport ]] ||
      die "the controller has no teleport issuer; run NVKE_AUTHZ_DEMO_TELEPORT=true tilt up"
  fi
  build_mint
}

setup() {
  kubectl config get-contexts "${CTX}" >/dev/null 2>&1 || die "no kube context ${CTX}"
  "${HERE}/gen.sh" >/dev/null || die "gen.sh failed"
  build_mint
  rm -rf "${OUT}/take"
  mkdir -p "${OUT}/take"
  local start=${SECONDS}
  kubectl --context "${CTX}" -n nvke-authz-demo rollout restart deployment/keycloak >/dev/null || die "restarting Keycloak failed"
  kubectl --context "${CTX}" -n nvke-authz-demo rollout status deployment/keycloak --timeout=300s >/dev/null ||
    die "Keycloak did not become ready"
  wait_for 120 kc_jwks || die "Keycloak's JWKS is not reachable at ${KC} after the restart"
  echo "keycloak ready after $(( SECONDS - start ))s"
  if (( TELEPORT )); then
    start=${SECONDS}
    teleport sh -c 'rm -f /tmp/ops-*' || die "cannot reach the Teleport pod"
    teleport tctl create -f /etc/teleport/resources/resources.yaml >/dev/null || die "tctl create failed"
    wait_for 120 teleport_app_registered || die "the nvke app did not register with Teleport"
    echo "teleport ready after $(( SECONDS - start ))s"
  fi
  wait_for 60 alice_lists_clusters || die "alice cannot list clusters"
  echo ready
}

if (( SETUP )); then
  setup
  exit 0
fi

preflight
mkdir -p "${OUT}/take"
cd "${OUT}" || exit 2
START="$(rfc3339_ago 60)"
export START

echo "# 1. alice is in nvke-admins, which grants admin."
show 'ALICE=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=alice -d password=alice-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $ALICE" "$API/clusters"' | tee take/code
check 1 "alice lists clusters: 200" [ "$(tail -1 take/code)" = 200 ]

echo
echo "# 2. vera is in nvke-viewers, which grants viewer: read, not write."
show 'VERA=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=vera -d password=vera-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $VERA" "$API/clusters"' | tee take/code
check 2a "vera lists clusters: 200" [ "$(tail -1 take/code)" = 200 ]
CLUSTER_ID="$(uuidgen | tr '[:upper:]' '[:lower:]')"
export CLUSTER_ID
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $VERA" -X DELETE "$API/clusters/$CLUSTER_ID"' | tee take/code
show 'cat take/body.json'
check 2b "vera deletes a cluster: 403" [ "$(tail -1 take/code)" = 403 ]
show 'VERA_SUB=$(jq -rR '\''split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson | .sub'\'' <<<"$VERA"); echo "$VERA_SUB"'
refusal_logged() {
  kubectl --context "${CTX}" -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time="${START}" |
    jq -cR --arg sub "$1" --arg action "$2" 'fromjson? | select(.msg == "refused request" and .subject == $sub and .action == $action)' |
    grep -q .
}
wait_for 10 refusal_logged "${VERA_SUB}" deleteCluster
show 'kubectl --context $CTX -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time=$START | jq -cR --arg sub "$VERA_SUB" '\''fromjson? | select(.msg == "refused request" and .subject == $sub) | {msg, action, subject, issuer, resource_id}'\'' | tail -1'
check 2c "the refusal is logged with vera's subject and deleteCluster" refusal_logged "${VERA_SUB}" deleteCluster

echo
echo "# 3. nora is admitted but holds no role. zed is in no admitted group."
show 'NORA=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=nora -d password=nora-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $NORA" "$API/clusters"; cat take/body.json' | tee take/code
check 3a "nora lists clusters: 403 from authorization" grep -q '^403' take/code
NORA_SUB="$(jq -rR 'split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson | .sub' <<<"${NORA}")"
wait_for 10 refusal_logged "${NORA_SUB}" listClusters
check 3b "the refusal is logged as \"refused request\" for listClusters" refusal_logged "${NORA_SUB}" listClusters
show 'ZED=$(curl -sS --cacert ca.crt -d grant_type=password -d client_id=nvkectl -d username=zed -d password=zed-demo "$KC/protocol/openid-connect/token" | jq -r .access_token)'
T3="$(rfc3339_ago 60)"
export T3
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $ZED" "$API/clusters"; cat take/body.json' | tee take/code
check 3c "zed lists clusters: 403" grep -q '^403' take/code
check 3d "zed's body says not admitted" grep -q 'Not admitted' take/body.json
not_admitted_logged() {
  kubectl --context "${CTX}" -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time="${T3}" |
    jq -cR 'fromjson? | select(.msg == "rejected request" and .reason == "not_admitted" and .issuer == "keycloak")' | grep -q .
}
wait_for 10 not_admitted_logged
show 'kubectl --context $CTX -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time=$T3 | jq -cR '\''fromjson? | select(.msg == "rejected request") | {msg, reason, issuer, status}'\'' | tail -1'
check 3e "authentication logs zed's rejection with reason not_admitted" not_admitted_logged

echo
echo "# 4. A token from an issuer the controller does not trust."
export ROGUE_ISS
show 'ROGUE=$(./mint rogue --key rogue-key.pem --iss $ROGUE_ISS --sub mallory --groups nvke-admins)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $ROGUE" "$API/clusters"; cat take/body.json' | tee take/code
check 4 "the rogue issuer's token: 401 unknown_issuer" grep -q 'unknown_issuer' take/body.json

echo
echo "# 5. Expired, malformed and tampered tokens."
show 'curl -sS --cacert ca.crt "$KC/protocol/openid-connect/certs" -o take/certs.json'
show 'ISS=$(jq -rR '\''split(".")[1] | gsub("-";"+") | gsub("_";"/") | @base64d | fromjson | .iss'\'' <<<"$ALICE"); echo "$ISS"'
show 'EXPIRED=$(./mint sign --key realm-key.pem --jwks take/certs.json --iss "$ISS" --sub alice --groups nvke-admins --ttl -10m)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $EXPIRED" "$API/clusters"; cat take/body.json' | tee take/code
check 5a "an expired token: 401" grep -q '^401' take/code
check 5b "the body says expired" grep -q 'expired' take/body.json
show 'MALFORMED=$(./mint malformed)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $MALFORMED" "$API/clusters"; cat take/body.json' | tee take/code
check 5c "a malformed token: 401 bad_signature" grep -q 'bad_signature' take/body.json
show 'TAMPERED=$(./mint tamper --token "$VERA" --claim '\''groups=["nvke-admins"]'\'')'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $TAMPERED" "$API/clusters"; cat take/body.json' | tee take/code
check 5d "vera's token rewritten into nvke-admins: 401 bad_signature" grep -q 'bad_signature' take/body.json
show 'for t in "$ALICE" "$VERA" "$NORA" "$ZED" "$ROGUE" "$EXPIRED" "$TAMPERED"; do echo "${t##*.}"; done > take/signatures.txt'
show 'kubectl --context $CTX -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time=$START > take/controller.log'
show 'grep -cE "eyJ[A-Za-z0-9_-]+\.eyJ" take/controller.log; grep -cFf take/signatures.txt take/controller.log' | tee take/code
check 5e "no token and no signature appears in the controller's log" \
  [ "$(tail -2 take/code | paste -sd, -)" = "0,0" -a -n "$(grep -m1 'refused request' take/controller.log)" ]

echo
echo "# 6. Every operation in the API, with no token, as vera and as alice."
rm -f take/openapi.yaml take/operations.tsv
show 'curl -fsS http://localhost:8080/openapi.yaml -o take/openapi.yaml' || die "cannot fetch the API description"
yq -o=json '.paths' take/openapi.yaml |
  jq -r 'to_entries[] | .key as $p | .value | to_entries[] | select(.key | test("^(get|put|post|patch|delete)$")) | [(.key | ascii_upcase), $p, .value.operationId] | @tsv' \
    > take/operations.tsv
[[ -s take/operations.tsv ]] || die "found no operations in openapi.yaml; is yq mikefarah/yq?"
echo "$(wc -l < take/operations.tsv | tr -d ' ') operations. Each runs:"
echo '  curl -s -o take/body.json -w "%{http_code}" -X <method> -H "Authorization: Bearer <token>" [-H "Content-Type: application/json" -d <body>] "$API<path>"'
echo "Path IDs are random UUIDs. A 404 from the handler means authorization passed."

# The provider createCluster names. The handler refuses a provider this
# deployment does not support before it writes anything, so the demo names one
# of those. When every provider is supported, alice's createCluster is skipped.
UNSUPPORTED="$(controller_config | yq -o=json '.supportedInfraProviders' |
  jq -r '(["launchLayer", "infra", "nico"] - .)[0] // ""')"
case "${UNSUPPORTED}" in
  infra | nico) PROVIDER="\"${UNSUPPORTED}\":{}" ;;
  *) PROVIDER='"launchLayer":{"siteID":"authz-demo"}' ;;
esac

# body OPERATION: the smallest body the request validator accepts. None can
# create anything: createCluster names an unsupported provider, and every
# other write names a cluster or node pool that does not exist.
body() {
  case "$1" in
    createCluster) echo "{\"name\":\"authz-demo-probe\",\"controlPlaneKubernetesVersion\":\"1.34.2\",\"etcdEncrypted\":false,${PROVIDER}}" ;;
    createNodePool) printf '{"clusterID":"%s","name":"authz-demo-probe","nodeNamePrefix":"p","desiredNodeCount":0,"nico":{}}' "$(uuidgen | tr '[:upper:]' '[:lower:]')" ;;
    updateClusterSpireServer) echo '{"address":"192.0.2.1","port":30440}' ;;
    updateCluster | updateNodePool) echo '{}' ;;
  esac
}

# authorized CODE: the handler answered, so authorization let the call through.
authorized() { [[ "$1" =~ ^[2-4][0-9][0-9]$ && "$1" != 401 && "$1" != 403 ]]; }

# call TOKEN METHOD PATH OPERATION: the status code.
call() {
  local token="$1" method="$2" path="$3" op="$4" data
  local args=(-s -o take/body.json -w '%{http_code}' -X "${method}")
  [[ -n "${token}" ]] && args+=(-H "Authorization: Bearer ${token}")
  data="$(body "${op}")"
  [[ -n "${data}" ]] && args+=(-H "Content-Type: application/json" -d "${data}")
  curl "${args[@]}" "${API}${path}"
}

sweep_none=0 sweep_vera=0 sweep_alice=0
printf '\n%-38s %-7s %-9s %-6s %s\n' operation method "no token" vera alice
while IFS=$'\t' read -r method path op; do
  real_path="$(sed -E "s/\{[^}]+\}/$(uuidgen | tr '[:upper:]' '[:lower:]')/g" <<<"${path}")"
  none="$(call "" "${method}" "${real_path}" "${op}")"
  vera="$(call "${VERA}" "${method}" "${real_path}" "${op}")"
  note=""
  if [[ "${op}" == createCluster && -z "${UNSUPPORTED}" ]]; then
    alice=-
    note="skipped: every provider is supported, so this would create a cluster"
  else
    alice="$(call "${ALICE}" "${method}" "${real_path}" "${op}")"
    [[ "${method}" != GET ]] && authorized "${alice}" && note="authorized (handler answered ${alice})"
  fi
  printf '%-38s %-7s %-9s %-6s %s %s\n' "${op}" "${method}" "${none}" "${vera}" "${alice}" "${note}"
  [[ "${none}" == 401 ]] || sweep_none=1
  if [[ "${method}" == GET ]]; then
    authorized "${vera}" || sweep_vera=1
  else
    [[ "${vera}" == 403 ]] || sweep_vera=1
  fi
  [[ "${alice}" == - ]] || authorized "${alice}" || sweep_alice=1
done < take/operations.tsv
check 6a "every operation with no token: 401" [ "${sweep_none}" = 0 ]
check 6b "vera: every GET authorized, every write 403" [ "${sweep_vera}" = 0 ]
check 6c "alice: every operation authorized" [ "${sweep_alice}" = 0 ]

echo
echo "# 7. ci-bot is a client whose service account is in nvke-admins."
show 'CI=$(curl -sS --cacert ca.crt -d grant_type=client_credentials -d client_id=ci-bot -d client_secret=ci-bot-demo-secret "$KC/protocol/openid-connect/token" | jq -r .access_token)'
show 'curl -s -o take/body.json -w "%{http_code}\n" -H "Authorization: Bearer $CI" "$API/clusters"' | tee take/code
check 7 "ci-bot lists clusters: 200" [ "$(tail -1 take/code)" = 200 ]

if (( TELEPORT )); then
  echo
  echo "# 8. Teleport: the same roles through a Teleport app. Teleport signs the JWT."
  show 'for u in ops-admin ops-viewer ops-nobody; do kubectl --context $CTX -n nvke-authz-demo exec deploy/teleport -- tctl auth sign --user $u --app-name nvke --format tls --ttl 1h --overwrite -o /tmp/$u >/dev/null 2>&1; for f in crt key; do kubectl --context $CTX -n nvke-authz-demo exec deploy/teleport -- cat /tmp/$u.$f > take/$u.$f; done; done'
  show 'curl -s -o take/body.json -w "%{http_code}\n" --cacert ca.crt --resolve nvke.teleport.nvke-demo.local:3080:127.0.0.1 --cert take/ops-admin.crt --key take/ops-admin.key "$TP/api/clusters"' | tee take/code
  check 8a "ops-admin lists clusters: 200" [ "$(tail -1 take/code)" = 200 ]
  show 'curl -s -o take/body.json -w "%{http_code}\n" --cacert ca.crt --resolve nvke.teleport.nvke-demo.local:3080:127.0.0.1 --cert take/ops-viewer.crt --key take/ops-viewer.key -X DELETE "$TP/api/clusters/$CLUSTER_ID"; cat take/body.json' | tee take/code
  check 8b "ops-viewer deletes a cluster: 403" grep -q '^403' take/code
  wait_for 10 refusal_logged ops-viewer deleteCluster
  show 'kubectl --context $CTX -n nvke-controller logs deploy/nvke-controller -c nvke-controller --since-time=$START | jq -cR '\''fromjson? | select(.msg == "refused request" and .issuer == "teleport") | {msg, action, subject, issuer}'\'' | tail -1'
  check 8c "the refusal is logged with subject ops-viewer" refusal_logged ops-viewer deleteCluster
  show 'curl -s -o take/body.json -w "%{http_code}\n" --cacert ca.crt --resolve nvke.teleport.nvke-demo.local:3080:127.0.0.1 --cert take/ops-nobody.crt --key take/ops-nobody.key "$TP/api/clusters"; cat take/body.json' | tee take/code
  check 8d "ops-nobody lists clusters: 403 not admitted" grep -q 'Not admitted' take/body.json
fi

echo
echo "# 9. Authorization decisions since the controller started, deleteCluster shown."
show 'kubectl --context $CTX get --raw /api/v1/namespaces/nvke-controller/services/nvke-controller:metrics/proxy/metrics | grep -E "^nvke_authz_(decisions_total|enabled)" > take/metrics.txt; grep -E "enabled|deleteCluster" take/metrics.txt'
check 9a "nvke_authz_enabled reports enforcing" grep -q '^nvke_authz_enabled{mode="enforcing"} 1' take/metrics.txt
check 9b "nvke_authz_decisions_total counts deleteCluster denials" grep -q '^nvke_authz_decisions_total{action="deleteCluster",outcome="deny"}' take/metrics.txt

echo
if (( FAILED )); then
  echo "FAILED"
  exit 1
fi
echo "ALL PASSED"
