#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# List, stop, start and delete nico-sim VMs, including orphans that exist on
# the host (QEMU process, libvirt domain or instance directory) but that
# nico-sim no longer knows about.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
db="${NICO_SIM_DB_PATH:-$root/.dev/nico-sim.db}"
dir="$(dirname "$db")/instances"
url="${NICO_SIM_URL:-http://localhost:18080}"
simctl() { "$root/bin/nico-simctl" "$@" --base-url "$url"; }
os="$(uname -s)"
virsh=(virsh -c "${NICO_VMS_LIBVIRT_URI:-qemu:///session}")

usage() {
	cat <<EOF
Usage: $0 <command> [args]

  list                      Show nico-sim's and the host's view of every VM.
  delete <name-or-id>...    Delete VMs (orphans are cleaned up on the host).
  clean                     Delete every orphan.
  stop|start <name-or-id>...|--all

DB: $db  (override with NICO_SIM_DB_PATH)
URL: $url  (override with NICO_SIM_URL)
EOF
	exit 2
}

# sim_tsv prints "id name cluster state ip" for each VM nico-sim knows.
# It returns non-zero if nico-sim does not answer.
sim_tsv() {
	curl -fsS -m 5 -H 'Authorization: Bearer vms.sh' "$url/sim/instances" |
		jq -r '.[] | [.id, .name, (.labels["cluster.x-k8s.io/cluster-name"] // "-"), .observedState, (.ip // "-")] | @tsv'
}

# host_tsv prints "id pid" for each VM the host knows, pid being "-" when no
# process or running domain was found.
host_tsv() {
	{
		if [[ $os == Darwin ]]; then
			ps -axo pid=,command= |
				awk '$2 ~ /qemu-system/ && match($0, /-name nico-vm-[^ ]+/) { print substr($0, RSTART + 14, RLENGTH - 14) "\t" $1 }'
		else
			"${virsh[@]}" list --all --name | sed -n 's/^nico-vm-//p' |
				while read -r id; do printf '%s\t%s\n' "$id" "$("${virsh[@]}" domid "nico-vm-$id" 2>/dev/null || echo -)"; done
		fi
		[[ -d $dir ]] && find "$dir" -mindepth 1 -maxdepth 1 -exec basename {} \; | awk '{ print $0 "\t-" }'
	} | sort -t$'\t' -k1,1 -k2,2r | awk -F'\t' '!seen[$1]++'
}

# resolve prints the full instance ID for a name, ID or ID prefix.
resolve() {
	local ref="$1" matches
	matches="$( { printf '%s\n' "$sim"; printf '%s\n' "$host"; } | awk -F'\t' -v r="$ref" '$1 == r || ($2 == r && NF > 2) { print $1 }' | sort -u)"
	[[ -n $matches ]] || matches="$(cut -f1 <<<"$sim"$'\n'"$host" | sort -u | grep "^$ref" || true)"
	if [[ $(grep -c . <<<"$matches") -ne 1 ]]; then
		echo "'$ref' matches $(grep -c . <<<"$matches") VMs" >&2
		return 1
	fi
	echo "$matches"
}

# orphan_delete removes everything the host holds for an instance.
orphan_delete() {
	local id="$1" pgid
	echo "Cleaning up orphan $id"
	if [[ $os == Darwin ]]; then
		pgid="$(cat "$dir/$id/vmnet-client.pid" 2>/dev/null || true)"
		[[ -z $pgid ]] || kill -TERM -- "-$pgid" 2>/dev/null || true
		awk -F'\t' -v i="$id" '$1 == i && $2 != "-" { print $2 }' <<<"$host" | xargs kill 2>/dev/null || true
		pkill -f "swtpm.*$dir/$id" || true
		sleep 1
	else
		"${virsh[@]}" destroy "nico-vm-$id" 2>/dev/null || true
		"${virsh[@]}" undefine "nico-vm-$id" --nvram --tpm 2>/dev/null || true
	fi
	rm -rf "${dir:?}/$id"
}

list() {
	{
		printf 'NAME\tID\tCLUSTER\tSTATUS\tIP\tPID\tDISK\n'
		local id name cluster status ip pid disk
		while read -r id; do
			IFS=$'\t' read -r _ name cluster status ip <<<"$(grep "^$id"$'\t' <<<"$sim" || true)"
			if [[ -z ${name:-} ]]; then
				name=- cluster=- ip=- status=ORPHAN
				((sim_up)) || status=unmanaged
			fi
			pid="$(awk -F'\t' -v i="$id" '$1 == i { print $2 }' <<<"$host")"
			disk="$(du -sh "$dir/$id" 2>/dev/null | cut -f1 || true)"
			printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "${id:0:8}" "$cluster" "$status" "$ip" "${pid:--}" "${disk:--}"
		done < <(cut -f1 <<<"$sim"$'\n'"$host" | sort -u | grep .)
	} | column -t -s$'\t'
}

[[ $# -ge 1 ]] || usage
cmd="$1"
shift

sim_up=1
sim="$(sim_tsv)" || {
	echo "nico-sim is not answering at $url; showing the host's view only." >&2
	sim_up=0 sim=""
}
host="$(host_tsv)"

case "$cmd" in
list) list ;;
delete)
	[[ $# -ge 1 ]] || usage
	for ref in "$@"; do
		id="$(resolve "$ref")"
		if grep -q "^$id"$'\t' <<<"$sim"; then
			simctl instance delete "$id" >/dev/null && echo "Deleting $ref via nico-sim"
		else
			orphan_delete "$id"
		fi
	done
	;;
clean)
	((sim_up)) || { echo "refusing to clean: without nico-sim every VM would look like an orphan" >&2; exit 1; }
	for id in $(cut -f1 <<<"$host" | grep -vxFf <(cut -f1 <<<"$sim") || true); do
		orphan_delete "$id"
	done
	;;
stop | start)
	((sim_up)) || exit 1
	if [[ ${1:-} == --all ]]; then
		# shellcheck disable=SC2046
		set -- $(cut -f1 <<<"$sim")
	else
		[[ $# -ge 1 ]] || usage
	fi
	for ref in "$@"; do
		id="$(resolve "$ref")"
		simctl instance "$cmd" "$id" >/dev/null && echo "$cmd $ref"
	done
	;;
*) usage ;;
esac
