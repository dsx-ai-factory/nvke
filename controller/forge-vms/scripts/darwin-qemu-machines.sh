#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

usage() {
	echo "Usage: $0 <list|clean>"
	echo ""
	echo "  list   List running darwin-qemu machines created by forge-vms."
	echo "  clean  Stop matching qemu processes and remove their instance artifacts."
}

trim_quotes() {
	local value="${1:-}"
	value="${value#\"}"
	value="${value%\"}"
	value="${value#\'}"
	value="${value%\'}"
	printf '%s' "${value}"
}

extract_process_metadata() {
	local cmd="$1"
	local instance=""
	local workdir=""
	local qmp_sock=""
	local token=""
	local next=""
	local i
	local -a tokens=()

	read -r -a tokens <<<"${cmd}"
	for ((i = 0; i < ${#tokens[@]}; i++)); do
		token="${tokens[$i]}"
		case "${token}" in
		-name)
			if ((i + 1 < ${#tokens[@]})); then
				next="$(trim_quotes "${tokens[$((i + 1))]}")"
				if [[ "${next}" == forge-vm-* ]]; then
					instance="${next#forge-vm-}"
				fi
			fi
			;;
		-qmp)
			if ((i + 1 < ${#tokens[@]})); then
				next="$(trim_quotes "${tokens[$((i + 1))]}")"
				if [[ "${next}" == unix:* ]]; then
					qmp_sock="${next#unix:}"
					qmp_sock="${qmp_sock%%,*}"
				fi
			fi
			;;
		-drive)
			if ((i + 1 < ${#tokens[@]})); then
				next="$(trim_quotes "${tokens[$((i + 1))]}")"
				if [[ "${next}" == *"file="*"/disk.qcow2"* ]]; then
					next="${next#*file=}"
					next="${next%%,*}"
					if [[ "${next}" == */disk.qcow2 ]]; then
						workdir="${next%/disk.qcow2}"
					fi
				fi
			fi
			;;
		esac
	done

	printf '%s\t%s\t%s\n' "${instance}" "${workdir}" "${qmp_sock}"
}

stop_pid() {
	local pid="$1"
	local i
	if ! kill -0 "${pid}" 2>/dev/null; then
		return 0
	fi

	if ! kill "${pid}" 2>/dev/null; then
		if kill -0 "${pid}" 2>/dev/null; then
			echo "Failed to send SIGTERM to pid ${pid}" >&2
			return 1
		fi
		return 0
	fi

	for i in {1..50}; do
		if ! kill -0 "${pid}" 2>/dev/null; then
			return 0
		fi
		sleep 0.2
	done

	if ! kill -9 "${pid}" 2>/dev/null; then
		if kill -0 "${pid}" 2>/dev/null; then
			echo "Failed to send SIGKILL to pid ${pid}" >&2
			return 1
		fi
		return 0
	fi

	for i in {1..20}; do
		if ! kill -0 "${pid}" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done

	echo "Process ${pid} still running after SIGKILL" >&2
	return 1
}

remove_instance_dir() {
	local instance_id="$1"
	local dir="$2"

	if [[ ! -d "${dir}" ]]; then
		return 0
	fi

	if [[ "$(basename "$(dirname "${dir}")")" != "instances" ]]; then
		echo "Skipping unsafe directory (parent is not 'instances'): ${dir}" >&2
		return 1
	fi
	if [[ "$(basename "${dir}")" != "${instance_id}" ]]; then
		echo "Skipping unsafe directory (unexpected basename): ${dir}" >&2
		return 1
	fi

	rm -rf "${dir}"
	return 0
}

if [[ $# -ne 1 ]]; then
	usage
	exit 1
fi

mode="$1"
case "${mode}" in
list | clean) ;;
*)
	usage
	exit 1
	;;
esac

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "darwin-qemu cleanup targets are only supported on Darwin hosts."
	exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
db_path="${FORGE_VMS_DB_PATH:-${repo_root}/var/forge-vms.db}"
if [[ "${db_path}" != /* ]]; then
	db_path="${repo_root}/${db_path}"
fi
instances_root="$(dirname "${db_path}")/instances"
tmp_dir="${TMPDIR:-/tmp}"
tmp_dir="${tmp_dir%/}"

scratch_dir="$(mktemp -d -t forge-vms-darwin-qemu.XXXXXX)"
trap 'rm -rf "${scratch_dir}"' EXIT
running_file="${scratch_dir}/running.tsv"
ids_file="${scratch_dir}/ids.txt"
sorted_ids_file="${scratch_dir}/ids.sorted.txt"
ps_file="${scratch_dir}/ps.txt"

: >"${running_file}"
if ! ps -axww -o pid= -o command= >"${ps_file}" 2>/dev/null; then
	echo "Failed to read process table via ps; cannot inspect darwin-qemu machines." >&2
	exit 1
fi

while IFS= read -r line; do
	# Trim leading whitespace from ps output.
	line="${line#"${line%%[![:space:]]*}"}"
	[[ -z "${line}" ]] && continue

	pid="${line%% *}"
	cmd="${line#* }"

	[[ "${cmd}" == *"qemu-system"* ]] || continue
	[[ "${cmd}" == *"forge-vm-"* ]] || continue

	meta="$(extract_process_metadata "${cmd}")"
	IFS=$'\t' read -r instance_id workdir qmp_sock <<<"${meta}"
	[[ -n "${instance_id}" ]] || continue
	if [[ -n "${workdir}" && "${workdir}" != /* ]]; then
		workdir="${repo_root}/${workdir}"
	fi

	printf '%s\t%s\t%s\t%s\n' "${pid}" "${instance_id}" "${workdir}" "${qmp_sock}" >>"${running_file}"
done <"${ps_file}"

if [[ "${mode}" == "list" ]]; then
	if [[ ! -s "${running_file}" ]]; then
		echo "No running darwin-qemu machines created by forge-vms were found."
		exit 0
	fi

	printf "%-8s %-36s %-48s %s\n" "PID" "INSTANCE_ID" "WORK_DIR" "QMP_SOCKET"
	while IFS=$'\t' read -r pid instance_id workdir qmp_sock; do
		printf "%-8s %-36s %-48s %s\n" "${pid}" "${instance_id}" "${workdir}" "${qmp_sock}"
	done <"${running_file}"
	exit 0
fi

: >"${ids_file}"

if [[ -s "${running_file}" ]]; then
	awk -F'\t' '{print $2}' "${running_file}" >>"${ids_file}"
fi

if [[ -d "${instances_root}" ]]; then
	for dir in "${instances_root}"/*; do
		[[ -d "${dir}" ]] || continue
		if [[ -f "${dir}/qemu.pid" || -f "${dir}/disk.qcow2" || -f "${dir}/seed.iso" || -f "${dir}/user-data" ]]; then
			basename "${dir}" >>"${ids_file}"
		fi
	done
fi

if [[ ! -s "${ids_file}" ]]; then
	echo "No darwin-qemu forge-vms machines or artifacts were found."
	exit 0
fi

sort -u "${ids_file}" >"${sorted_ids_file}"

had_error=0
cleaned=0
while IFS= read -r instance_id; do
	[[ -z "${instance_id}" ]] && continue
	row="$(awk -F'\t' -v id="${instance_id}" '$2 == id {print; exit}' "${running_file}")"

	pid=""
	workdir=""
	qmp_sock=""
	if [[ -n "${row}" ]]; then
		IFS=$'\t' read -r pid _ workdir qmp_sock <<<"${row}"
	fi
	if [[ -z "${workdir}" ]]; then
		workdir="${instances_root}/${instance_id}"
	fi

	echo "Cleaning forge-vm-${instance_id}"
	if [[ -n "${pid}" ]]; then
		if stop_pid "${pid}"; then
			echo "  stopped pid ${pid}"
		else
			echo "  failed to stop pid ${pid}" >&2
			had_error=1
		fi
	fi

	if [[ -n "${qmp_sock}" && -e "${qmp_sock}" ]]; then
		if rm -f "${qmp_sock}"; then
			echo "  removed qmp socket ${qmp_sock}"
		else
			echo "  failed to remove qmp socket ${qmp_sock}" >&2
			had_error=1
		fi
	fi

	short_id="${instance_id:0:12}"
	for sock in "${tmp_dir}"/fvm-*-"${short_id}".sock; do
		[[ -e "${sock}" ]] || continue
		if rm -f "${sock}"; then
			echo "  removed temp qmp socket ${sock}"
		else
			echo "  failed to remove temp qmp socket ${sock}" >&2
			had_error=1
		fi
	done

	if [[ -f "${workdir}/qemu.pid" ]]; then
		rm -f "${workdir}/qemu.pid" || true
	fi
	if [[ -d "${workdir}" ]]; then
		if remove_instance_dir "${instance_id}" "${workdir}"; then
			echo "  removed artifacts directory ${workdir}"
		else
			had_error=1
		fi
	fi
	cleaned=$((cleaned + 1))
done <"${sorted_ids_file}"

echo "Processed ${cleaned} machine(s)."
if [[ "${had_error}" -ne 0 ]]; then
	exit 1
fi
