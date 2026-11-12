#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Generate the authorization demo's keys, certificates and controller values
# into controller/.local/authz-demo (gitignored), and print that directory.
# Existing keys and certificates are kept, so a rerun changes nothing; delete
# the directory to start over.
#
#   ca.crt, ca.key              the demo CA; every jwks.caBundle and curl --cacert
#   keycloak.crt, keycloak.key  Keycloak's serving certificate, leaf then CA
#   teleport.crt, teleport.key  Teleport's proxy certificate, leaf then CA
#   realm-key.pem, realm.crt    the nvke realm's RS256 signing key and certificate
#   realm-key.b64, realm.b64    the same as one-line base64, for realm-nvke.json
#   rogue-key.pem               an ES256 key no issuer publishes, for mint rogue
#   values.yaml                 apiAuth issuers and apiAuthz for Keycloak
#   values-teleport.yaml        the same plus the Teleport issuer
#
# Usage: hack/authz-demo/gen.sh [output-dir]

set -euo pipefail

controller_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
out="${1:-${controller_dir}/.local/authz-demo}"
mkdir -p "${out}"
chmod 700 "${out}"

namespace="nvke-authz-demo"
teleport_cluster="teleport.nvke-demo.local"
# The Teleport app's uri. Teleport sets a token's aud to exactly this string.
teleport_app_uri="http://nvke-controller.nvke-controller.svc.cluster.local:8080"

# serving_cert NAME SANS: a P-256 leaf signed by the demo CA.
serving_cert() {
  local name="$1" sans="$2"
  [[ -f "${out}/${name}.crt" && -f "${out}/${name}.key" ]] && return 0
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "${out}/${name}.key" -out "${out}/${name}.csr" -subj "/CN=${name}.${namespace}.svc" 2>/dev/null
  printf 'subjectAltName=%s\nbasicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' \
    "${sans}" > "${out}/${name}.ext"
  openssl x509 -req -in "${out}/${name}.csr" -CA "${out}/ca.crt" -CAkey "${out}/ca.key" -CAcreateserial \
    -out "${out}/${name}.leaf" -days 365 -sha256 -extfile "${out}/${name}.ext" 2>/dev/null
  cat "${out}/${name}.leaf" "${out}/ca.crt" > "${out}/${name}.crt"
  # Keycloak and Teleport run as other users and read the key from a Secret.
  chmod 644 "${out}/${name}.key"
  rm -f "${out}/${name}.csr" "${out}/${name}.ext" "${out}/${name}.leaf" "${out}/ca.srl"
}

if [[ ! -f "${out}/ca.crt" || ! -f "${out}/ca.key" ]]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "${out}/ca.key" -out "${out}/ca.crt" -days 3650 \
    -subj "/CN=nvke-authz-demo CA" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
  chmod 600 "${out}/ca.key"
  rm -f "${out}"/keycloak.* "${out}"/teleport.*
fi

serving_cert keycloak \
  "DNS:keycloak.${namespace}.svc,DNS:keycloak.${namespace}.svc.cluster.local,DNS:keycloak.${namespace},DNS:localhost,IP:127.0.0.1"
serving_cert teleport \
  "DNS:${teleport_cluster},DNS:*.${teleport_cluster},DNS:teleport.${namespace}.svc,DNS:teleport.${namespace}.svc.cluster.local,DNS:localhost,IP:127.0.0.1"

if [[ ! -f "${out}/realm-key.pem" ]]; then
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${out}/realm-key.pem" 2>/dev/null
  chmod 600 "${out}/realm-key.pem"
  rm -f "${out}/realm.crt"
fi
if [[ ! -f "${out}/realm.crt" ]]; then
  openssl req -x509 -key "${out}/realm-key.pem" -out "${out}/realm.crt" -days 3650 -subj "/CN=nvke" 2>/dev/null
fi
# put FILE: write stdin to FILE only if it differs. Tilt reloads the Tiltfile
# when a file it reads changes, and the Tiltfile runs this script.
put() {
  local tmp
  tmp="$(mktemp "$1.XXXXXX")"
  cat > "${tmp}"
  if cmp -s "${tmp}" "$1"; then rm -f "${tmp}"; else mv "${tmp}" "$1"; fi
}

# Keycloak's realm import substitutes these into a JSON string, so one line.
grep -v -- '-----' "${out}/realm-key.pem" | tr -d '\n' | put "${out}/realm-key.b64"
grep -v -- '-----' "${out}/realm.crt" | tr -d '\n' | put "${out}/realm.b64"

if [[ ! -f "${out}/rogue-key.pem" ]]; then
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${out}/rogue-key.pem" 2>/dev/null
  chmod 600 "${out}/rogue-key.pem"
fi

# The values files are rewritten every run; they depend only on ca.crt.
ca_bundle="$(sed 's/^/            /' "${out}/ca.crt")"

keycloak_issuer="$(cat <<EOF
      - name: keycloak
        issuer: "https://keycloak.${namespace}.svc/realms/nvke"
        audiences: [nvke-controller]
        jwks:
          uri: "https://keycloak.${namespace}.svc/realms/nvke/protocol/openid-connect/certs"
          caBundle: |
${ca_bundle}
        algorithms: [RS256]
        admit:
          groups: [nvke-admins, nvke-viewers, nvke-users]
EOF
)"

teleport_issuer="$(cat <<EOF
      - name: teleport
        issuer: "${teleport_cluster}"
        audiences: ["${teleport_app_uri}"]
        jwks:
          uri: "https://teleport.${namespace}.svc:3080/.well-known/jwks.json"
          caBundle: |
${ca_bundle}
        algorithms: [ES256]
        admit:
          claims:
            - {name: roles, value: nvke-admin}
            - {name: roles, value: nvke-viewer}
EOF
)"

# write_values FILE ISSUERS AUTHZ_ISSUERS
write_values() {
  put "$1" <<EOF
# Generated by hack/authz-demo/gen.sh. The issuers list replaces the one in
# hack/tilt-controller-api-auth-values.yaml, so local-kubernetes is repeated.
apiAuth:
  config:
    issuers:
      - name: local-kubernetes
        issuer: "https://kubernetes.default.svc.cluster.local"
        jwks:
          uri: "https://kubernetes.default.svc/openid/v1/jwks"
          caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
          bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
        algorithms: [RS256, ES256]
        admit:
          serviceAccounts:
            - namespace: nke-clients
              name: nke-client
            - namespace: management-engine-system
              name: management-engine-controller-manager
$2
apiAuthz:
  enabled: true
  permissive: false
  authorizer:
    localBundle:
      config:
        issuers:
          keycloak:
            claims:
              - {name: groups, value: nvke-admins, grants: [admin]}
              - {name: groups, value: nvke-viewers, grants: [viewer]}
          local-kubernetes:
            serviceAccounts:
              - {namespace: nke-clients, name: nke-client, grants: [admin]}
              - {namespace: management-engine-system, name: management-engine-controller-manager, grants: [admin]}
$3
EOF
}

write_values "${out}/values.yaml" "${keycloak_issuer}" ""
write_values "${out}/values-teleport.yaml" "${keycloak_issuer}
${teleport_issuer}" "$(cat <<'EOF'
          teleport:
            claims:
              - {name: roles, value: nvke-admin, grants: [admin]}
              - {name: roles, value: nvke-viewer, grants: [viewer]}
EOF
)"

echo "${out}"
