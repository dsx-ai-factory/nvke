#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Renders SPIRE agent config from cloud-init instance identity. See os-image/SPEC.md.

set -euo pipefail

ENV_FILE=/etc/nke/spire.env
BUNDLE_SRC=/etc/nke/trust-bundle.pem
BUNDLE_DST=/etc/spire-agent/trust-bundle.pem
CONF_TMPL=/usr/lib/nke/spire-agent.conf.tmpl
CONF_DST=/etc/spire-agent/agent.conf
DROP_IN_DIR=/etc/systemd/system/spire-agent.service.d
ATTESTED_MARKER=/var/lib/spire-agent/.nke-attested
TPM_PLUGIN=/usr/local/lib/spire-agent/tpm_attestor_agent

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "nke-spire-configure: missing ${ENV_FILE}" >&2
  exit 1
fi
if [[ ! -f "${BUNDLE_SRC}" ]]; then
  echo "nke-spire-configure: missing ${BUNDLE_SRC}" >&2
  exit 1
fi

# shellcheck disable=SC1090
set -a
source "${ENV_FILE}"
set +a

: "${SPIRE_SERVER_ADDRESS:?nke-spire-configure: SPIRE_SERVER_ADDRESS is required}"
: "${SPIRE_SERVER_PORT:?nke-spire-configure: SPIRE_SERVER_PORT is required}"
: "${SPIRE_TRUST_DOMAIN:?nke-spire-configure: SPIRE_TRUST_DOMAIN is required}"

id -u spire >/dev/null 2>&1 || useradd -r -s /bin/false -d /var/lib/spire-agent spire

install -d -m 0755 /etc/spire-agent
install -d -m 0750 -o spire -g spire /var/lib/spire-agent /var/lib/spire-agent/keys
install -d -m 0755 -o spire -g spire /tmp/spire-agent /tmp/spire-agent/public

install -m 0644 -o spire -g spire "${BUNDLE_SRC}" "${BUNDLE_DST}"

# Pick the node attestor the same way CWE picks it for CAPI-backed nodes
# (controller/internal/temporal/activities/capi_spire_agent.conf.tmpl): a join
# token when cloud-init supplied one, TPM otherwise. A site cluster always
# attests by TPM and mints no token, so an image that only knew join_token could
# never join one.
if [[ -n "${SPIRE_JOIN_TOKEN:-}" ]]; then
  attestor='  NodeAttestor "join_token" {
    plugin_data {}
  }'
elif [[ -x "${TPM_PLUGIN}" ]]; then
  # SPIRE verifies the plugin binary against this checksum before running it.
  tpm_checksum="$(sha256sum "${TPM_PLUGIN}" | cut -d' ' -f1)"
  attestor="  NodeAttestor \"tpm\" {
    plugin_cmd = \"${TPM_PLUGIN}\"
    plugin_checksum = \"${tpm_checksum}\"
    plugin_data {}
  }"
else
  echo "nke-spire-configure: no SPIRE_JOIN_TOKEN and ${TPM_PLUGIN} is missing" >&2
  exit 1
fi

umask 022
awk -v attestor="${attestor}" '
  { gsub(/@SPIRE_NODE_ATTESTOR@/, attestor); print }
' "${CONF_TMPL}" |
sed \
  -e "s|@SPIRE_SERVER_ADDRESS@|${SPIRE_SERVER_ADDRESS}|g" \
  -e "s|@SPIRE_SERVER_PORT@|${SPIRE_SERVER_PORT}|g" \
  -e "s|@SPIRE_TRUST_DOMAIN@|${SPIRE_TRUST_DOMAIN}|g" \
  >"${CONF_DST}"
chown spire:spire "${CONF_DST}"
chmod 0644 "${CONF_DST}"

install -d -m 0755 "${DROP_IN_DIR}"
JOIN_DROPIN="${DROP_IN_DIR}/join-token.conf"
if [[ ! -f "${ATTESTED_MARKER}" && -n "${SPIRE_JOIN_TOKEN:-}" ]]; then
  umask 077
  cat >"${JOIN_DROPIN}" <<EOF
[Service]
EnvironmentFile=${ENV_FILE}
ExecStart=
ExecStart=/usr/local/bin/spire-agent run -config /etc/spire-agent/agent.conf -joinToken \${SPIRE_JOIN_TOKEN}
EOF
  chmod 0600 "${JOIN_DROPIN}"
else
  rm -f "${JOIN_DROPIN}"
fi

systemctl daemon-reload
