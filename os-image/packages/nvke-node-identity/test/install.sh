#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Qualify nvke-node-identity's install, upgrade and remove on a disposable
# systemd host (the native CI runner). Run as root:
#
#   sudo test/install.sh <path-to-built.deb>
#
# It stands in for nvke-agent and spire-agent with minimal units, because what
# is under test is how systemd merges the packaged drop-in with a unit that
# lives in /etc, as the agent's does. The upgrade cases start from what
# provision.sh used to leave on an image, so they also prove an image built
# before this package updates in place.
set -euo pipefail

deb="${1:?pass the absolute path to the built Debian package}"
if [ "$(id -u)" != 0 ]; then
  echo "Package qualification requires root on a disposable host" >&2
  exit 1
fi
if [ "$(dpkg-query -W -f='${db:Status-Status}' nvke-node-identity 2>/dev/null || true)" = installed ]; then
  echo "Refusing to replace an existing nvke-node-identity outside the test fixture" >&2
  exit 1
fi
if [ -e /etc/systemd/system/nvke-agent.service ] || [ -e /etc/systemd/system/spire-agent.service ]; then
  echo "Refusing to replace an existing nvke-agent or spire-agent unit outside the test fixture" >&2
  exit 1
fi

script=/usr/lib/nvke/wait-spiffe-socket.sh
dropin=/lib/systemd/system/nvke-agent.service.d/50-nvke-identity.conf
old_dir=/etc/systemd/system/nvke-agent.service.d
old=${old_dir}/10-spire.conf
# sha256 of the old drop-in as provision.sh installed it. postinst retires the
# file by this hash, so the check below fails if the two ever drift apart.
old_sha=ca162afb5ddfe490e8b64e463addadfa2bf749ac3a6ff082345649acc3c79b3f

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  rm -f /etc/systemd/system/nvke-agent.service /etc/systemd/system/spire-agent.service
  rm -rf "${old_dir}"
  systemctl daemon-reload >/dev/null 2>&1 || true
}
trap cleanup EXIT

write_old_dropin() {
  install -d -m 0755 "${old_dir}"
  # The exact bytes provision.sh installed, copied from the file as it was in
  # os-image/files/nvke-agent-spire.conf before this package.
  cat >"${old}" <<'OLD'
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

[Unit]
# Replace the package's After=multi-user.target to avoid an ordering cycle
# with the target that enables the service.
After=
After=spire-agent.service
Wants=spire-agent.service

[Service]
ExecStartPre=/usr/lib/nvke/wait-spiffe-socket.sh
ExecStartPost=/usr/bin/touch /var/lib/spire-agent/.nvke-attested
OLD
}

show() { systemctl show nvke-agent.service "$@"; }

assert_ordered_after_spire() {
  test -e "${dropin}" || fail "${dropin} is missing"
  test -x "${script}" || fail "${script} is not executable"
  for path in "${script}" "${dropin}"; do
    test "$(dpkg-query -S "${path}" | cut -d: -f1)" = nvke-node-identity || fail "${path} is not owned by nvke-node-identity"
  done
  test ! -e "${old}" || fail "${old} was not retired"
  # systemd merges the drop-in in /lib with a unit in /etc, and it is the only
  # one. It reports the canonical path, /usr/lib on a merged-/usr system.
  test "$(show -p DropInPaths --value)" = "$(readlink -f "${dropin}")" || fail "DropInPaths is '$(show -p DropInPaths --value)', not just ${dropin}"
  # spire-agent is in the ordering. (The drop-in's empty After= does not remove
  # the unit's own After=multi-user.target on systemd 255 or 259, so this does
  # not assert that.)
  after="$(show -p After --value)"
  case " ${after} " in *" spire-agent.service "*) ;; *) fail "nvke-agent is not ordered after spire-agent: ${after}" ;; esac
  case " $(show -p Wants --value) " in *" spire-agent.service "*) ;; *) fail "nvke-agent does not want spire-agent" ;; esac
  show -p ExecStartPre | grep -F "${script}" >/dev/null || fail "ExecStartPre does not run ${script}"
  show -p ExecStartPost | grep -F "/usr/bin/touch /var/lib/spire-agent/.nvke-attested" >/dev/null || fail "ExecStartPost does not touch the attestation marker"
  # Exactly once: a leftover old drop-in would add it a second time.
  # systemctl show prints each command as path=... ; argv[]=..., so count path=.
  test "$(show -p ExecStartPre | grep -o "path=${script}" | wc -l)" = 1 || fail "ExecStartPre does not run ${script} exactly once"
}

# 1. A node built by provision.sh: the agent unit, from its package, in /etc with
#    After=multi-user.target, and the old drop-in beside it.
cat >/etc/systemd/system/spire-agent.service <<'UNIT'
[Unit]
Description=stand-in for spire-agent
[Service]
ExecStart=/bin/sleep infinity
UNIT
cat >/etc/systemd/system/nvke-agent.service <<'UNIT'
[Unit]
Description=stand-in for nvke-agent
After=multi-user.target
[Service]
ExecStart=/bin/sleep infinity
[Install]
WantedBy=multi-user.target
UNIT
write_old_dropin
test "$(sha256sum "${old}" | cut -d' ' -f1)" = "${old_sha}" || fail "the legacy ${old} does not match the file provision.sh installed"
systemctl daemon-reload
# Before the package: the old drop-in already orders the agent after SPIRE.
case " $(show -p After --value) " in *" spire-agent.service "*) ;; *) fail "the fixture's old drop-in does not order the agent after spire-agent" ;; esac

# 2. Upgrade from that node.
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_ordered_after_spire
test ! -d "${old_dir}" || fail "the emptied ${old_dir} was left behind"

# 3. Reinstalling the same version changes nothing.
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_ordered_after_spire

# 4. An edited old drop-in is left alone, and says so.
install -d -m 0755 "${old_dir}"
printf '[Service]\nEnvironment=OPERATOR_EDIT=1\n' >"${old}"
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}" 2>&1 | grep -F 'was edited, so it is left in place' >/dev/null ||
  fail "an edited ${old} was not reported"
test -f "${old}" || fail "an edited ${old} was removed"
rm -rf "${old_dir}"
systemctl daemon-reload

# 5. A same-named file in /etc replaces the packaged drop-in: operators keep control.
install -d -m 0755 "${old_dir}"
printf '[Service]\nEnvironment=OVERRIDDEN=1\n' >"${old_dir}/50-nvke-identity.conf"
systemctl daemon-reload
case " $(show -p After --value) " in *" spire-agent.service "*) fail "an /etc/.../50-nvke-identity.conf did not replace the packaged drop-in" ;; *) ;; esac
rm -rf "${old_dir}"
systemctl daemon-reload
assert_ordered_after_spire

# 6. Remove: the files are gone and systemd no longer orders the agent after spire.
dpkg -r nvke-node-identity
for path in "${script}" "${dropin}"; do
  test ! -e "${path}" || fail "${path} survived remove"
done
case " $(show -p After --value) " in *" spire-agent.service "*) fail "nvke-agent is still ordered after spire-agent after remove" ;; *) ;; esac

# 7. Install after an image that never had the old file, then purge.
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_ordered_after_spire
dpkg -P nvke-node-identity
test ! -e "${dropin}" || fail "${dropin} survived purge"

echo "PASS: nvke-node-identity install, upgrade and remove"
