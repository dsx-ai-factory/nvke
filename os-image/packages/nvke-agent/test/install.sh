#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Qualify package replacement on a disposable systemd host (the native CI runner).
set -euo pipefail

repo_root="$(cd -- "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
deb="${1:?pass the absolute path to the built Debian package}"
arch="$(dpkg --print-architecture)"
if [ "$(id -u)" != 0 ]; then
  echo "Package qualification requires root on a disposable host" >&2
  exit 1
fi
for package in nke-agent nvke-agent; do
  if [ "$(dpkg-query -W -f='${db:Status-Status}' "$package" 2>/dev/null || true)" = installed ]; then
    echo "Refusing to replace an existing $package outside the test fixture" >&2
    exit 1
  fi
done
for path in /var/lib/nke /var/lib/nvke /usr/local/nke /usr/local/nvke /etc/default/nke-agent /etc/default/nvke-agent; do
  if [ -e "$path" ] || [ -L "$path" ]; then
    echo "Refusing to overwrite pre-existing agent data at $path" >&2
    exit 1
  fi
done
work=$(mktemp -d)
mkdir -p "$work/old/DEBIAN" "$work/old/usr/local/bin" "$work/old/etc/systemd/system"
cat > "$work/old/DEBIAN/control" <<CONTROL
Package: nke-agent
Version: 0.2.0
Architecture: ${arch}
Maintainer: NVKE test
Description: legacy agent fixture for rename qualification
CONTROL
cat > "$work/old/usr/local/bin/nke-agent" <<'BINARY'
#!/bin/sh
case "$1" in --version|-version) echo 'NKE Agent Version: v0.2.0'; exit 0;; esac
exec sleep infinity
BINARY
chmod 755 "$work/old/usr/local/bin/nke-agent"
cp "$work/old/usr/local/bin/nke-agent" "$work/old/usr/local/bin/nke-agent-updater"
cp "${repo_root}/agent/scripts/nvke-agent.service" "$work/old/etc/systemd/system/nke-agent.service"
sed -i 's/nvke-agent/nke-agent/g;/^Alias=/d' "$work/old/etc/systemd/system/nke-agent.service"
cp "${repo_root}/agent/scripts/nvke-agent-updater@.service" "$work/old/etc/systemd/system/nke-agent-updater@.service"
sed -i 's/nvke-agent/nke-agent/g' "$work/old/etc/systemd/system/nke-agent-updater@.service"
cp "${repo_root}/os-image/packages/nvke-agent/packaging/prerm" "$work/old/DEBIAN/prerm"
sed -i 's/nvke-agent/nke-agent/g;/systemctl disable/d' "$work/old/DEBIAN/prerm"
chmod 755 "$work/old/DEBIAN/prerm"
dpkg-deb --build "$work/old" "$work/nke-agent_0.2.0_${arch}.deb" >/dev/null
dpkg -i "$work/nke-agent_0.2.0_${arch}.deb"
mkdir -p /var/lib/nke/agent-upgrades/retained /usr/local/nke/user-scripts /etc/nke
printf 'AGENT_ARGS="--disable-mtls"\n' >/etc/default/nke-agent
printf '{"nodeName":"rename-test","agentVersion":"v0.2.0"}\n' >/var/lib/nke/state.json
printf rotation-checkpoint >/var/lib/nke/enc-rotation.state
printf retained-operation >/var/lib/nke/agent-upgrades/retained/operation.json
printf user-script >/usr/local/nke/user-scripts/check.sh
printf controller-owned >/etc/nke/rename-test
systemctl daemon-reload
systemctl enable --now nke-agent.service
systemctl is-active --quiet nke-agent.service
old_state=$(sha256sum /var/lib/nke/state.json | cut -d' ' -f1)
# Both the real package and systemd run here; only starting the new daemon is deferred.
NVKE_AGENT_SKIP_START=1 DEBIAN_FRONTEND=noninteractive apt-get -y --allow-downgrades install "$deb"
[ "$(dpkg-query -W -f='${db:Status-Status}' nvke-agent)" = installed ]
for field in Replaces Conflicts Provides; do
  [ "$(dpkg-deb -f "$deb" "$field")" = nke-agent ]
done
[ "$(systemctl show nke-agent.service -p Id --value)" = nvke-agent.service ]
[ "$(sha256sum /var/lib/nvke/state.json | cut -d' ' -f1)" = "$old_state" ]
[ "$(cat /var/lib/nvke/enc-rotation.state)" = rotation-checkpoint ]
[ "$(cat /var/lib/nvke/agent-upgrades/retained/operation.json)" = retained-operation ]
[ "$(cat /usr/local/nvke/user-scripts/check.sh)" = user-script ]
[ "$(cat /etc/nke/rename-test)" = controller-owned ]
[ /etc/default/nke-agent -ef /etc/default/nvke-agent ]
/usr/local/bin/nke-agent --version | grep -E '^NVKE Agent Version: [^ ]+'
systemctl start nvke-agent.service
systemctl is-active --quiet nvke-agent.service
systemctl status nke-agent.service --no-pager
# Retain the old deb, restore it through apt and confirm the old daemon's state path.
DEBIAN_FRONTEND=noninteractive apt-get -y --allow-downgrades install "$work/nke-agent_0.2.0_${arch}.deb"
systemctl daemon-reload
systemctl enable --now nke-agent.service
systemctl is-active --quiet nke-agent.service
[ "$(cat /var/lib/nke/enc-rotation.state)" = rotation-checkpoint ]
[ "$(cat /var/lib/nke/agent-upgrades/retained/operation.json)" = retained-operation ]
# Qualify the actual independent updater with both bundle layouts too.
# Requests use an exact release version, so stamp this private fixture explicitly.
target_version=v0.3.0
make -C "$repo_root/agent" "build-${arch}" VERSION="$target_version"
bundle_archive="$work/agent.tgz"
tar -czf "$bundle_archive" \
  -C "$repo_root/agent/bin/linux_${arch}" nvke-agent nvke-agent-updater \
  -C "$repo_root/agent/scripts" nvke-agent.service nvke-agent-updater@.service migrate-agent-paths.sh
op_root="$work/operations"
upgrade_request="${target_version}_bundle"
downgrade_request=v0.2.0_bundle
mkdir -p "$op_root/$upgrade_request" "$op_root/$downgrade_request"
printf '{"targetVersion":"%s","previousVersion":"v0.2.0","method":"bundle","artifactUrl":"file://%s","phase":"Pending"}\n' \
  "$target_version" "$bundle_archive" >"$op_root/$upgrade_request/operation.json"
"$repo_root/agent/bin/linux_${arch}/nvke-agent-updater" --operation-root "$op_root" --request "$upgrade_request"
grep -q '"phase": "Succeeded"' "$op_root/$upgrade_request/operation.json"
systemctl is-active --quiet nvke-agent.service
older_archive="$work/older-agent.tgz"
tar -czf "$older_archive" -C "$work/old/usr/local/bin" nke-agent -C "$work/old/etc/systemd/system" nke-agent.service
printf '{"targetVersion":"v0.2.0","previousVersion":"%s","method":"bundle","artifactUrl":"file://%s","phase":"Pending"}\n' \
  "$target_version" "$older_archive" >"$op_root/$downgrade_request/operation.json"
"$repo_root/agent/bin/linux_${arch}/nvke-agent-updater" --operation-root "$op_root" --request "$downgrade_request"
grep -q '"phase": "Succeeded"' "$op_root/$downgrade_request/operation.json"
systemctl is-active --quiet nke-agent.service
/usr/local/bin/nke-agent --version | grep -Fx 'NKE Agent Version: v0.2.0'
[ "$(cat /var/lib/nvke/enc-rotation.state)" = rotation-checkpoint ]
echo 'PASS: apt replacement, systemd alias, state preservation, native daemon startup, bundle upgrade and legacy bundle downgrade'
