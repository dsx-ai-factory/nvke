#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# site-sampler.sh OUT
#
# Appends a timeline sample to OUT/timeline/timeline.log every interval: the
# clock, the CNI configuration directory, the default route, and once the site
# kubeconfig exists, every pod with its address, the kube-dns endpoints and
# the ovnkube-node containers' readiness. The e2e suite runs it as a systemd
# unit from before the bootstrap until the run ends, and
# collect-site-bundle.sh folds the log into the failure bundle. Which network
# a pod landed on, and when, is a question only a timeline answers.
#
# Reads only. Exits 0 after SITE_SAMPLER_MAX_SECONDS so an abandoned node
# does not sample forever; the unit's Restart=on-failure leaves that exit alone.
set -u

OUT=${1:-/var/lib/nke-e2e/site-bundle}
KUBECONFIG_PATH=${SITE_KUBECONFIG:-/var/lib/nke-e2e/site.kubeconfig}
INTERVAL=${SITE_SAMPLER_INTERVAL:-15}
MAX_SECONDS=${SITE_SAMPLER_MAX_SECONDS:-21600}
LOG=$OUT/timeline/timeline.log
ROTATE_BYTES=8388608
START_EPOCH=$(date +%s)

mkdir -p "$OUT/timeline"

while [ $(($(date +%s) - START_EPOCH)) -lt "$MAX_SECONDS" ]; do
	if [ -f "$LOG" ] && [ "$(wc -c <"$LOG" | tr -d ' ')" -gt "$ROTATE_BYTES" ]; then
		mv -f "$LOG" "$LOG.1"
	fi
	{
		echo "=== $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "--- /etc/cni/net.d"
		ls -la /etc/cni/net.d 2>&1
		echo "--- default route"
		ip -4 route show default 2>&1
		if [ -r "$KUBECONFIG_PATH" ]; then
			echo "--- pods"
			timeout 10 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=8s get pods -A -o wide 2>&1
			echo "--- kube-dns endpoints"
			timeout 10 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=8s -n kube-system get endpoints kube-dns -o wide 2>&1
			echo "--- ovnkube-node containers"
			timeout 10 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=8s -n ovn-kubernetes get pods -l name=ovnkube-node -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.containerStatuses[*]}{.name}={.ready}{" "}{end}{"\n"}{end}' 2>&1
		fi
	} >>"$LOG" 2>&1
	sleep "$INTERVAL"
done
