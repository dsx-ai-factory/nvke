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
TPM_CONF_TMPL=/usr/lib/nke/spire-agent-tpm.conf.tmpl
CONF_DST=/etc/spire-agent/agent.conf
TPM_PLUGIN=/usr/local/lib/spire-agent/tpm_attestor_agent
DROP_IN_DIR=/etc/systemd/system/spire-agent.service.d
ATTESTED_MARKER=/var/lib/spire-agent/.nke-attested

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "nke-spire-configure: missing ${ENV_FILE}" >&2
  exit 1
fi
if [[ ! -f "${BUNDLE_SRC}" ]]; then
  echo "nke-spire-configure: missing ${BUNDLE_SRC}" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

: "${SPIRE_SERVER_ADDRESS:?nke-spire-configure: SPIRE_SERVER_ADDRESS is required}"
: "${SPIRE_SERVER_PORT:?nke-spire-configure: SPIRE_SERVER_PORT is required}"
: "${SPIRE_TRUST_DOMAIN:?nke-spire-configure: SPIRE_TRUST_DOMAIN is required}"
SPIRE_ATTESTATION_METHOD="${SPIRE_ATTESTATION_METHOD:-join_token}"
TPM_PLUGIN_CHECKSUM=""
case "${SPIRE_ATTESTATION_METHOD}" in
  join_token)
    ;;
  tpm)
    if [[ ! -x "${TPM_PLUGIN}" ]]; then
      echo "nke-spire-configure: missing ${TPM_PLUGIN}" >&2
      exit 1
    fi
    CONF_TMPL="${TPM_CONF_TMPL}"
    TPM_PLUGIN_CHECKSUM="$(sha256sum "${TPM_PLUGIN}" | cut -d' ' -f1)"
    ;;
  *)
    echo "nke-spire-configure: unsupported SPIRE_ATTESTATION_METHOD=${SPIRE_ATTESTATION_METHOD}" >&2
    exit 1
    ;;
esac

id -u spire >/dev/null 2>&1 || useradd -r -s /bin/false -d /var/lib/spire-agent spire
if [[ "${SPIRE_ATTESTATION_METHOD}" == tpm ]]; then
  if ! getent group tss >/dev/null; then
    echo "nke-spire-configure: tss group is missing; install tpm2-tools" >&2
    exit 1
  fi
  usermod -aG tss spire
fi

install -d -m 0755 /etc/spire-agent
install -d -m 0750 -o spire -g spire /var/lib/spire-agent /var/lib/spire-agent/keys
install -d -m 0755 -o spire -g spire /tmp/spire-agent /tmp/spire-agent/public

install -m 0644 -o spire -g spire "${BUNDLE_SRC}" "${BUNDLE_DST}"

umask 022
sed \
  -e "s|@SPIRE_SERVER_ADDRESS@|${SPIRE_SERVER_ADDRESS}|g" \
  -e "s|@SPIRE_SERVER_PORT@|${SPIRE_SERVER_PORT}|g" \
  -e "s|@SPIRE_TRUST_DOMAIN@|${SPIRE_TRUST_DOMAIN}|g" \
  -e "s|@TPM_PLUGIN_CHECKSUM@|${TPM_PLUGIN_CHECKSUM}|g" \
  "${CONF_TMPL}" >"${CONF_DST}"
chown spire:spire "${CONF_DST}"
chmod 0644 "${CONF_DST}"

install -d -m 0755 "${DROP_IN_DIR}"
JOIN_DROPIN="${DROP_IN_DIR}/join-token.conf"
if [[ "${SPIRE_ATTESTATION_METHOD}" == join_token && ! -f "${ATTESTED_MARKER}" && -n "${SPIRE_JOIN_TOKEN:-}" ]]; then
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
