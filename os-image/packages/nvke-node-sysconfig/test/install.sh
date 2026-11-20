#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Qualify nvke-node-sysconfig's install, upgrade and remove on a disposable
# systemd host (the native CI runner). Run as root:
#
#   sudo test/install.sh <path-to-built.deb>
#
# The upgrade cases start from what provision.sh used to leave on an image, so
# they also prove an image built before this package updates in place.
set -euo pipefail

deb="${1:?pass the absolute path to the built Debian package}"
if [ "$(id -u)" != 0 ]; then
  echo "Package qualification requires root on a disposable host" >&2
  exit 1
fi
if [ "$(dpkg-query -W -f='${db:Status-Status}' nvke-node-sysconfig 2>/dev/null || true)" = installed ]; then
  echo "Refusing to replace an existing nvke-node-sysconfig outside the test fixture" >&2
  exit 1
fi
if id -u etcd >/dev/null 2>&1 || [ -e /var/lib/etcd ]; then
  echo "Refusing to touch an existing etcd user or /var/lib/etcd outside the test fixture" >&2
  exit 1
fi

old_timeouts=/etc/sysctl.d/10-nke-conntrack-timeouts.conf
old_modules=/etc/modules-load.d/nf_conntrack.conf
old_unit=/etc/systemd/system/conntrack-tune.service
new_timeouts=/usr/lib/sysctl.d/50-nvke-conntrack-timeouts.conf
new_modules=/usr/lib/modules-load.d/50-nvke-conntrack.conf
new_unit=/lib/systemd/system/conntrack-tune.service
new_sysusers=/usr/lib/sysusers.d/50-nvke-etcd.conf
new_tmpfiles=/usr/lib/tmpfiles.d/50-nvke-etcd.conf
script=/usr/lib/nvke/configure-conntrack.sh
generated=/run/sysctl.d/50-nvke-conntrack.conf
old_generated=/run/sysctl.d/10-nke-conntrack.conf
old_operator=/etc/sysctl.d/10-nke-conntrack.conf
etcd_dir=/var/lib/etcd

# Every unit the package masks.
masked=(multipathd.service multipathd.socket apt-daily.timer apt-daily-upgrade.timer
  apt-daily.service apt-daily-upgrade.service unattended-upgrades.service ovs-exporter.service)

repo_root="$(cd -- "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
files="${repo_root}/os-image/files"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

owned_by_package() {
  [ "$(dpkg-query -S "$1" | cut -d: -f1)" = nvke-node-sysconfig ] || fail "$1 is not owned by nvke-node-sysconfig"
}

assert_installed_state() {
  for path in "${script}" "${new_unit}" "${new_timeouts}" "${new_modules}" "${new_sysusers}" "${new_tmpfiles}"; do
    owned_by_package "${path}"
  done
  test -x "${script}" || fail "${script} is not executable"
  # A copy under /etc would mask the vendor unit.
  test ! -e "${old_unit}" || fail "${old_unit} masks the vendor unit"
  test ! -e "${old_timeouts}" || fail "${old_timeouts} was not retired"
  test ! -e "${old_modules}" || fail "${old_modules} was not retired"
  test ! -e "${old_generated}" || fail "${old_generated} was not retired"
  test "$(systemctl is-enabled conntrack-tune.service)" = enabled || fail "conntrack-tune.service is not enabled"
  systemctl is-active --quiet conntrack-tune.service || fail "conntrack-tune.service is not active"
  # The packaged TIME_WAIT value is the one the kernel is using.
  test "$(sysctl -n net.netfilter.nf_conntrack_tcp_timeout_time_wait)" = 60 || fail "TIME_WAIT is not 60"

  # etcd: a system user with no login, and a 0700 directory it owns.
  test "$(getent passwd etcd | cut -d: -f7)" = /bin/false || fail "the etcd user has a login shell"
  test "$(getent passwd etcd | cut -d: -f3)" -lt 1000 || fail "the etcd user is not a system user"
  test "$(stat -c '%U:%G %a' "${etcd_dir}")" = 'etcd:etcd 700' || fail "${etcd_dir} is not etcd:etcd 0700"

  # The masks: package-owned links to /dev/null that systemd reads as masked,
  # and none of those units is running.
  for unit in "${masked[@]}"; do
    test "$(readlink "/etc/systemd/system/${unit}")" = /dev/null || fail "${unit} is not a link to /dev/null"
    owned_by_package "/etc/systemd/system/${unit}"
    test "$(systemctl is-enabled "${unit}" 2>&1 || true)" = masked || fail "${unit} is not masked"
    if systemctl is-active --quiet "${unit}"; then
      fail "${unit} is running"
    fi
  done
}

# 1. A node that has been running: the background apt timers are enabled and
#    active, and /var/lib/etcd is the root:root directory an earlier kubeadm init
#    made, holding data. On top of what provision.sh left for conntrack.
install -d -m 0755 /usr/lib/nvke
install -m 0755 "${files}/configure-conntrack.sh" "${script}"
install -m 0644 "${files}/conntrack-tune.service" "${old_unit}"
# The pre-package bytes of the timeouts file are rebuilt from the renamed one:
# only its header and one comment word changed. postinst retires the old file by
# this hash, so the check below also fails if the two ever drift apart.
sed -e 's#^\# Installed by the nvke-node-sysconfig package as#\# Installed by the NVKE reference image as#' \
  -e 's#^\# /usr/lib/sysctl.d/50-nvke-conntrack-timeouts.conf. See os-image/SPEC.md.#\# /etc/sysctl.d/10-nke-conntrack-timeouts.conf. See os-image/SPEC.md.#' \
  -e 's#The 50- prefix sorts#The 10- prefix sorts#' \
  "${files}/50-nvke-conntrack-timeouts.conf" >"${old_timeouts}"
test "$(sha256sum "${old_timeouts}" | cut -d' ' -f1)" = cb2f42623e9407aa55bfe5fb009869d5bd2f1cad0048b03596a9ae5fd408e800 ||
  fail "the rebuilt legacy ${old_timeouts} does not match the file provision.sh installed"
echo nf_conntrack >"${old_modules}"
# A generated file from before the rename, still in /run on a running node.
install -d -m 0755 /run/sysctl.d
printf "net.netfilter.nf_conntrack_max = 600000\n" >"${old_generated}"
install -d -m 0755 "${etcd_dir}/member"
printf 'wal' >"${etcd_dir}/member/data"
chmod 0755 "${etcd_dir}"
systemctl daemon-reload
systemctl enable --now apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
systemctl is-active --quiet apt-daily.timer || echo "note: apt-daily.timer is not active on this host, so the stop path is not exercised"

# 2. Upgrade from that node.
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_installed_state
# The directory is corrected; what is inside it is not touched.
test "$(cat "${etcd_dir}/member/data")" = wal || fail "the etcd data was changed"
test "$(stat -c '%U' "${etcd_dir}/member/data")" = root || fail "the etcd data's owner was changed"

# 3. Reinstalling the same version changes nothing.
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_installed_state

# 3b. A directory that drifted is corrected again at install, not only at boot.
chown root:root "${etcd_dir}"
chmod 0755 "${etcd_dir}"
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_installed_state

# 4. An edited legacy timeouts file is left alone, and says so.
printf 'net.netfilter.nf_conntrack_tcp_timeout_time_wait = 30\n' >"${old_timeouts}"
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}" 2>&1 | grep -F 'was edited, so it is left in place' >/dev/null ||
  fail "an edited ${old_timeouts} was not reported"
test -f "${old_timeouts}" || fail "an edited ${old_timeouts} was removed"
rm -f "${old_timeouts}"

# 4b. An operator's file under the old generated name replaces nothing now, and says so.
printf 'net.netfilter.nf_conntrack_max = 1000000\n' >"${old_operator}"
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}" 2>&1 | grep -F 'no longer replaces the generated file' >/dev/null ||
  fail "a file at ${old_operator} was not reported"
rm -f "${old_operator}"

# 5. Remove: the unit is off, the generated file is gone, the package files and the
#    masks are gone. The etcd user and directory stay: that is node data.
dpkg -r nvke-node-sysconfig
for path in "${script}" "${new_unit}" "${new_timeouts}" "${new_modules}" "${new_sysusers}" "${new_tmpfiles}" "${generated}"; do
  test ! -e "${path}" || fail "${path} survived remove"
done
for unit in "${masked[@]}"; do
  test ! -e "/etc/systemd/system/${unit}" || fail "the mask on ${unit} survived remove"
done
test "$(systemctl is-enabled conntrack-tune.service 2>&1 || true)" != enabled || fail "conntrack-tune.service is still enabled after remove"
test "$(cat "${etcd_dir}/member/data")" = wal || fail "remove deleted the etcd data"
id -u etcd >/dev/null || fail "remove deleted the etcd user"

# 6. An image baked by provision.sh: the masks are the links `systemctl mask`
#    wrote, unowned by any package. The package must take them over.
for unit in "${masked[@]}"; do
  systemctl mask "${unit}" >/dev/null 2>&1
done
DEBIAN_FRONTEND=noninteractive dpkg -i "${deb}"
assert_installed_state

# 7. Purge. Same as remove: node data stays.
dpkg -P nvke-node-sysconfig
test ! -e "${new_unit}" || fail "${new_unit} survived purge"
for unit in "${masked[@]}"; do
  test ! -e "/etc/systemd/system/${unit}" || fail "the mask on ${unit} survived purge"
done
test "$(cat "${etcd_dir}/member/data")" = wal || fail "purge deleted the etcd data"

echo "PASS: nvke-node-sysconfig install, upgrade and remove"
