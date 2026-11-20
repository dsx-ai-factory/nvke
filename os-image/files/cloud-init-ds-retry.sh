#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Retries cloud-init's datasource discovery when the first boot found none.
#
# Production nodes boot with `ds=nocloud-net;s=<seed-url>` on the kernel
# command line (the iPXE `cloud-init-ds` variable). If the network is down at
# that moment -- a DPU or HBN wedge -- cloud-init tries the seed once, gives
# up, and pins the instance to DataSourceNone for good: no user-data, no
# capiPreKubeadmCommands, no nvke-agent. Nothing on the node notices when the
# network comes back, so it stays stranded until an operator runs
# `cloud-init clean` and re-runs the init and module stages by hand.
# See https://github.com/dsx-ai-factory/nvke/issues/609.
#
# Run by nvke-cloud-init-ds-retry.service on the nvke-cloud-init-ds-retry.timer
# cadence. Each run:
#
#   1. exits at once when there is nothing to do: the done sentinel exists,
#      or the kernel command line carries no `s=` seed URL, meaning no
#      network datasource was ever asked for;
#   2. exits while cloud-init is still running -- `extended_status` is only
#      read once it settles (done/error);
#   3. while the datasource is still none, probes <seed>/meta-data -- the
#      same object cloud-init fetches -- and exits until it answers;
#   4. once it answers, checks no cloud-init stage is still in flight, then
#      re-runs the datasource fetch exactly like the documented operator
#      recovery: `cloud-init clean`, `init`, and the config and final module
#      stages.
#
# The timer cannot loop forever: probing stops after MAX_PROBES ticks without
# an answer, and a recovery that leaves the node on DataSourceNone again --
# the seed answers but serves nothing usable -- is retried up to
# MAX_RECOVERIES times. Either exhaustion writes the sentinel and logs to the
# journal: by then the node's networking or seed is an operator problem, and
# re-cleaning state every timer tick helps nobody. Deciding also clears the
# spent budgets, so deleting the sentinel and re-enabling the timer re-arms
# the unit for a fresh cycle:
#   rm /var/lib/nvke/cloud-init-ds-retry.done
#   systemctl enable --now nvke-cloud-init-ds-retry.timer
#
# Everything below is overridable for the test suite; production always uses
# the defaults. The NVKE_DSRETRY_TEST_* prefix keeps a stray production
# environment from redirecting state paths or binaries.

set -euo pipefail

CMDLINE="${NVKE_DSRETRY_TEST_CMDLINE:-/proc/cmdline}"
STATE_DIR="${NVKE_DSRETRY_TEST_STATE_DIR:-/var/lib/nvke}"
CLOUD_INIT="${NVKE_DSRETRY_TEST_CLOUD_INIT:-cloud-init}"
PYTHON="${NVKE_DSRETRY_TEST_PYTHON:-python3}"
SYSTEMCTL="${NVKE_DSRETRY_TEST_SYSTEMCTL:-systemctl}"

readonly TIMER_UNIT="nvke-cloud-init-ds-retry.timer"
readonly DONE_MARK="${STATE_DIR}/cloud-init-ds-retry.done"
readonly ATTEMPTS_FILE="${STATE_DIR}/cloud-init-ds-retry.attempts"
readonly PROBES_FILE="${STATE_DIR}/cloud-init-ds-retry.probes"
readonly MAX_RECOVERIES=6
# 60 ticks at the timer's 2-minute cadence is about two hours of down-seed
# probing -- long enough to outlast a DPU/HBN bring-up, short enough that a
# permanently dark seed does not keep the unit alive forever.
readonly MAX_PROBES=60

log() {
  echo "cloud-init-ds-retry: $*"
}

# mark_done leaves a sentinel so later runs exit immediately, clears the
# spent budgets so the documented re-arm (delete the sentinel, re-enable
# the timer) starts a fresh cycle rather than instantly giving up again,
# and disables the timer so a decided node does not keep paying for a
# check it no longer needs.
mark_done() {
  install -d -m 0755 "${STATE_DIR}"
  install -m 0644 /dev/null "${DONE_MARK}"
  rm -f "${ATTEMPTS_FILE}" "${PROBES_FILE}"
  "${SYSTEMCTL}" disable --now "${TIMER_UNIT}" >/dev/null 2>&1 || true
}

# seed_from_cmdline prints the s= value of the ds=nocloud[-net] kernel
# argument, if any. The iPXE chain writes `ds=nocloud-net;s=<url>`; cloud-init
# also treats `ds=nocloud` with an http(s) seed as a network datasource, so
# both spellings count.
seed_from_cmdline() {
  local tok rest pair pairs toks=()
  read -ra toks <"${CMDLINE}" 2>/dev/null || true
  for tok in "${toks[@]}"; do
    case "${tok}" in
      ds=nocloud|ds=nocloud\;*|ds=nocloud-net|ds=nocloud-net\;*)
        rest="${tok#ds=}"
        local IFS=';'
        read -ra pairs <<<"${rest}"
        for pair in "${pairs[@]}"; do
          case "${pair}" in
            s=*) printf '%s' "${pair#s=}"; return 0 ;;
          esac
        done
        ;;
    esac
  done
  return 0
}

# cloud_init_report prints `extended_status` and `datasource` from
# `cloud-init status --format=yaml` as one pipe-separated line, e.g.
# "degraded done|none" -- `|` is a non-whitespace delimiter, so an empty
# datasource field stays put and neither value can contain it. The extended
# status is what actually says cloud-init settled: it is "done", "degraded
# done" or "error - done" only once every stage has finished -- plain
# `status` can already read "error" while a later stage still runs.
cloud_init_report() {
  local yaml extended datasource
  yaml="$("${CLOUD_INIT}" status --format=yaml 2>/dev/null || true)"
  extended="$(awk '/^extended_status:/{sub(/^extended_status:[[:space:]]*/, ""); print; exit}' <<<"${yaml}")"
  datasource="$(awk '/^datasource:/{sub(/^datasource:[[:space:]]*/, ""); print; exit}' <<<"${yaml}")"
  printf '%s|%s\n' "${extended}" "${datasource}"
}

# datasource_norm lowercases the value and removes every whitespace run, so
# the comparisons below work on both shapes cloud-init reports: the short
# "nocloud"/"none" and the long "DataSourceNoCloudNet [seed=...]" /
# "DataSourceNone".
datasource_norm() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]'
}

# datasource_is_none matches the datasource-absent spellings: "none" and
# "DataSourceNone" (optionally with trailing attributes).
datasource_is_none() {
  case "$(datasource_norm "$1")" in
    none|datasourcenone*) return 0 ;;
    *) return 1 ;;
  esac
}

# datasource_known reports whether the value is a real datasource name
# rather than an unset or YAML null marker.
datasource_known() {
  case "$1" in
    ""|"''"|"null"|"~") return 1 ;;
    *) return 0 ;;
  esac
}

# cloud_init_idle reports that no cloud-init stage is in flight. The stage
# units (cloud-init-local, cloud-init, cloud-init-main, cloud-config,
# cloud-final -- the set varies by cloud-init version) are Type=oneshot with
# RemainAfterExit=yes: once the boot finishes they sit at "active (exited)"
# indefinitely, so `is-active` can never witness idle -- it returns 0 on
# them forever. A stage actually doing work is in the activating (or
# deactivating) state while its ExecStart runs; that is the only moment
# `cloud-init clean` must not delete /var/lib/cloud. list-units with a state
# filter prints matching units and --quiet drops the legend, so non-empty
# output means busy. A failed query also prints nothing, so it must be
# treated as busy rather than idle: clean must never run when the stage
# state is unreadable.
cloud_init_idle() {
  local units
  units="$("${SYSTEMCTL}" list-units --type=service --state=activating,deactivating 'cloud-*.service' --quiet 2>/dev/null)" || return 1
  [[ -z "${units}" ]]
}

# seed_reachable checks the seed URL serves meta-data -- the document
# cloud-init itself fetches. python3 is baked into the image; curl and wget
# are not, so the probe is a stdlib urllib call.
seed_reachable() {
  local url="$1"
  "${PYTHON}" - "${url}" <<'PYEOF'
import sys
import urllib.request

try:
    with urllib.request.urlopen(sys.argv[1], timeout=10) as response:
        sys.exit(0 if 200 <= response.status < 400 else 1)
except Exception:
    sys.exit(1)
PYEOF
}

# count_in prints the integer stored in a state file, or 0.
count_in() {
  local file="$1" value
  value="$(cat "${file}" 2>/dev/null || true)"
  if [[ "${value}" =~ ^[0-9]+$ ]]; then
    printf '%s' "${value}"
  else
    printf '0'
  fi
}

# bump increments the integer stored in a state file.
bump() {
  local file="$1" value
  value="$(count_in "${file}")"
  install -d -m 0755 "${STATE_DIR}"
  printf '%s\n' "$((value + 1))" >"${file}"
}

main() {
  if [[ -e "${DONE_MARK}" ]]; then
    exit 0
  fi

  local seedfrom
  seedfrom="$(seed_from_cmdline)"
  if [[ -z "${seedfrom}" ]]; then
    log "no s= seed URL on the kernel command line; nothing to retry"
    mark_done
    exit 0
  fi
  case "${seedfrom}" in
    http://*|https://*) ;;
    *)
      # /cidata, file:// and other local seeds have nothing the network
      # probe can wait for: either cloud-init already used them or no
      # amount of retrying will make them reachable.
      log "seed '${seedfrom}' is not an http(s) URL; nothing the retry can wait for"
      mark_done
      exit 0
      ;;
  esac

  local extended datasource
  IFS='|' read -r extended datasource <<<"$(cloud_init_report)"
  case "${extended}" in
    *done) ;;
    disabled*)
      log "cloud-init is disabled; nothing to retry"
      mark_done
      exit 0
      ;;
    *)
      # not-started/running or an empty report: cloud-init has not settled,
      # so the datasource cannot be judged yet.
      exit 0
      ;;
  esac

  if ! datasource_is_none "${datasource}"; then
    if datasource_known "${datasource}"; then
      # A real datasource claimed the node; nothing left to retry.
      mark_done
    fi
    # Unset or null: cloud-init finished without recording a datasource at
    # all; keep checking rather than deciding this is terminal.
    exit 0
  fi

  # cloud-init joins the seed URL with a forward slash and fetches
  # meta-data, user-data, vendor-data and network-config under it. A query
  # or fragment is not part of the seed prefix; strip both so the probe URL
  # stays well-formed.
  local seed_base="${seedfrom%%#*}"
  seed_base="${seed_base%%\?*}"
  local seed_url="${seed_base%/}/meta-data"

  local probes
  probes="$(count_in "${PROBES_FILE}")"
  if (( probes >= MAX_PROBES )); then
    log "datasource is DataSourceNone and ${seed_url} never answered in ${MAX_PROBES} probes; giving up"
    mark_done
    exit 0
  fi
  bump "${PROBES_FILE}"
  if ! seed_reachable "${seed_url}"; then
    log "datasource is DataSourceNone and ${seed_url} is not reachable yet (probe $((probes + 1))/${MAX_PROBES})"
    exit 0
  fi

  local attempts
  attempts="$(count_in "${ATTEMPTS_FILE}")"
  if (( attempts >= MAX_RECOVERIES )); then
    log "${seed_url} answers but ${MAX_RECOVERIES} recoveries left the node on DataSourceNone; giving up"
    mark_done
    exit 0
  fi

  if ! cloud_init_idle; then
    # extended_status said done, but a cloud-init stage went back to work
    # while the probe ran, or the stage state could not be read at all --
    # clean must not delete /var/lib/cloud under a live stage.
    log "a cloud-init stage is in flight or its state could not be read; will retry on the next timer"
    exit 0
  fi

  log "datasource is DataSourceNone and ${seed_url} answers; re-running cloud-init (recovery $((attempts + 1))/${MAX_RECOVERIES})"
  bump "${ATTEMPTS_FILE}"
  # No --logs: the failed boot's log is failure evidence an operator still
  # needs, and journald already holds this unit's own output.
  if ! "${CLOUD_INIT}" clean; then
    log "cloud-init clean failed; will retry on the next timer"
    exit 0
  fi
  if ! "${CLOUD_INIT}" init; then
    log "cloud-init init failed; will retry on the next timer"
    exit 0
  fi
  if ! "${CLOUD_INIT}" modules --mode=config; then
    log "cloud-init modules --mode=config failed; will retry on the next timer"
    exit 0
  fi
  if ! "${CLOUD_INIT}" modules --mode=final; then
    log "cloud-init modules --mode=final failed; will retry on the next timer"
    exit 0
  fi

  IFS='|' read -r extended datasource <<<"$(cloud_init_report)"
  if [[ "${extended}" == *done ]] && datasource_known "${datasource}" && ! datasource_is_none "${datasource}"; then
    log "cloud-init claimed ${datasource}; done"
    mark_done
  else
    log "cloud-init still reports datasource '${datasource:-<unset>}' (status '${extended:-<unset>}'); will retry on the next timer"
  fi
}

main "$@"
