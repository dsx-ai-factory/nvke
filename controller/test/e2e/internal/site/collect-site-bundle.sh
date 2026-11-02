#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# collect-site-bundle.sh OUT ARCHIVE
#
# Collects the site node's state and logs and the site cluster's objects,
# events and pod logs into OUT, then tars OUT to ARCHIVE. The e2e suite stages
# this file onto the node and runs it as root, under `timeout`, when a spec
# fails; the archive is fetched, redacted and uploaded as a CI artifact.
#
# Rules this script follows, and that bundle_test.go checks:
#   - reads only: no kubectl verb that changes the cluster, no service restarts;
#   - every command runs through run/kube with its own timeout and byte cap,
#     and its exit status lands in index.tsv rather than stopping the rest;
#   - the deny list in the suite's scrub.go is never read: no kubeconfig
#     contents, no Secret objects, no helm values or manifests, no key material;
#   - the EXIT trap always writes the archive, so a deadline leaves a partial
#     bundle rather than none;
#   - one collector per OUT: the script takes a lock on it and exits 73 when
#     another run holds it, because a dropped SSH session does not kill the
#     sudo'd script on the node and two runs on one directory would overlap;
#   - what it writes on the node is root-only (umask 077): OUT and ARCHIVE are
#     the unredacted copies, and a kept node keeps them.
#
# Environment:
#   SITE_KUBECONFIG              admin kubeconfig on the node (default /var/lib/nke-e2e/site.kubeconfig)
#   SITE_CONTROLLER_NAMESPACE    where the NVKE controller is deployed (default nvke-controller)
#   SITE_BUNDLE_TIMELINE_DIR     where site-sampler.sh writes (default /var/lib/nke-e2e/site-timeline)
#   SITE_BUNDLE_DEADLINE_SECONDS the outer timeout, recorded in the manifest
#   SITE_BUNDLE_HOST_ROOT        test hook: prefix for every file the script reads
set -u

OUT=${1:?usage: collect-site-bundle.sh OUT ARCHIVE}
ARCHIVE=${2:?usage: collect-site-bundle.sh OUT ARCHIVE}
KUBECONFIG_PATH=${SITE_KUBECONFIG:-/var/lib/nke-e2e/site.kubeconfig}
CONTROLLER_NAMESPACE=${SITE_CONTROLLER_NAMESPACE:-nvke-controller}
TIMELINE_DIR=${SITE_BUNDLE_TIMELINE_DIR:-/var/lib/nke-e2e/site-timeline}
DEADLINE_SECONDS=${SITE_BUNDLE_DEADLINE_SECONDS:-0}
HOST_ROOT=${SITE_BUNDLE_HOST_ROOT:-}
umask 077

# The lock lives beside OUT, not in it, so it is not in the archive. Taken
# before the EXIT trap is set: a run that leaves here must not tar the other
# run's directory. A node without flock runs unlocked, as before.
mkdir -p "${OUT%/*}"
if command -v flock >/dev/null 2>&1; then
	exec 9>"$OUT.lock"
	if ! flock -n 9; then
		echo "another collector holds $OUT" >&2
		exit 73
	fi
fi

K=1024
M=1048576
INDEX=$OUT/index.tsv
MANIFEST=$OUT/manifest.txt
TMP=$OUT/.tmp
STARTED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
START_EPOCH=$(date +%s)
INTERRUPTED=no
API_REACHABLE=unknown
KUBECTL_TIMEOUT=20

# OUT and ARCHIVE are this run's alone; the sampler's timeline lives in
# TIMELINE_DIR and is copied in below. The old archive goes too, so a run that
# dies before its EXIT trap cannot hand the suite an earlier run's evidence.
rm -rf "$OUT"
rm -f "$ARCHIVE" "$ARCHIVE.partial"
mkdir -p "$OUT/node" "$OUT/cluster" "$TMP"
printf 'section\texit\tbytes\tseconds\tcommand\n' >"$INDEX"

# record SECTION EXIT BYTES SECONDS COMMAND...: one row of index.tsv.
record() {
	r_name=$1 r_exit=$2 r_bytes=$3 r_secs=$4
	shift 4
	printf '%s\t%s\t%s\t%s\t%s\n' "$r_name" "$r_exit" "$r_bytes" "$r_secs" "$(printf '%s ' "$@" | tr '\t\n' '  ')" >>"$INDEX"
}

# run SECTION SECONDS CAP COMMAND...: COMMAND under its own timeout, the first
# CAP bytes of its combined output in OUT/SECTION, its exit status in the index.
# The status travels through a file because a pipeline's status is head's.
# In the index, 124 is the timeout, 127 a tool the node does not have, and 141
# a command the cap cut off (it got SIGPIPE when head closed the pipe).
run() {
	name=$1 secs=$2 cap=$3
	shift 3
	out=$OUT/$name
	mkdir -p "${out%/*}"
	t0=$(date +%s)
	{
		timeout -k 5 "$secs" "$@" 2>&1
		echo $? >"$out.rc"
	} | head -c "$cap" >"$out"
	rc=$(cat "$out.rc" 2>/dev/null || echo '?')
	rm -f "$out.rc"
	record "$name" "$rc" "$(wc -c <"$out" | tr -d ' ')" $(($(date +%s) - t0)) "$@"
}

# kube SECTION SECONDS CAP ARGS...: kubectl against the site kubeconfig, bounded.
kube() {
	k_name=$1 k_secs=$2 k_cap=$3
	shift 3
	run "$k_name" "$k_secs" "$k_cap" kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout="${KUBECTL_TIMEOUT}s" "$@"
}

# copy SECTION CAP PATH: the last CAP bytes of a file, because the end is where
# a failure is. A missing file is a row with exit 1 and no bytes.
copy() {
	c_name=$1 c_cap=$2 c_path=$3
	src=$HOST_ROOT$c_path
	out=$OUT/$c_name
	mkdir -p "${out%/*}"
	if [ -r "$src" ] && [ -f "$src" ]; then
		tail -c "$c_cap" "$src" >"$out" 2>/dev/null
		rc=$?
	else
		: >"$out"
		rc=1
	fi
	record "$c_name" "$rc" "$(wc -c <"$out" | tr -d ' ')" 0 copy "$c_path"
}

# copydir SECTION CAP DIR [MAX]: every regular file directly under DIR, each
# capped, at most MAX of them.
copydir() {
	d_name=$1 d_cap=$2 d_dir=$3 d_max=${4:-50}
	src_dir=$HOST_ROOT$d_dir
	if [ ! -d "$src_dir" ]; then
		record "$d_name" 1 0 0 "missing directory $d_dir"
		return
	fi
	d_n=0
	for f in "$src_dir"/* "$src_dir"/.[!.]*; do
		[ -f "$f" ] || continue
		d_n=$((d_n + 1))
		if [ "$d_n" -gt "$d_max" ]; then
			record "$d_name/..." - 0 0 "capped at $d_max files"
			break
		fi
		copy "$d_name/$(basename "$f")" "$d_cap" "${f#"$HOST_ROOT"}"
	done
}

# skip SECTION REASON: a row for something deliberately not collected.
skip() {
	record "$1" - 0 0 "skipped: $2"
}

finish() {
	status=$?
	trap - EXIT
	[ "$status" -eq 124 ] && INTERRUPTED=deadline
	rm -rf "$TMP"
	# A file over 8 MiB keeps its tail, which is where the failure is.
	find "$OUT" -type f -size +8M ! -name 'timeline.log*' -exec sh -c 'tail -c 8388608 "$1" >"$1.tail" && mv -f "$1.tail" "$1"' _ {} \; 2>/dev/null
	{
		echo "collector: $(sha256sum "$0" 2>/dev/null | cut -d' ' -f1)"
		echo "started: $STARTED"
		echo "finished: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "elapsed-seconds: $(($(date +%s) - START_EPOCH))"
		echo "deadline-seconds: $DEADLINE_SECONDS"
		echo "interrupted: $INTERRUPTED"
		echo "host: $(hostname 2>/dev/null)"
		echo "kernel: $(uname -r 2>/dev/null)"
		echo "os: $(. "${HOST_ROOT}/etc/os-release" 2>/dev/null && echo "$PRETTY_NAME")"
		if [ -r "$KUBECONFIG_PATH" ]; then echo "kubeconfig: present"; else echo "kubeconfig: absent"; fi
		echo "api-reachable: $API_REACHABLE"
		echo "tools: kubectl=$(kubectl version --client 2>/dev/null | head -n1 | tr -d '\n') crictl=$(crictl --version 2>/dev/null | tr -d '\n') nerdctl=$(nerdctl --version 2>/dev/null | tr -d '\n') kind=$(kind --version 2>/dev/null | tr -d '\n') helm=$(helm version --short 2>/dev/null | tr -d '\n') ovs-vsctl=$(ovs-vsctl --version 2>/dev/null | head -n1 | tr -d '\n') nvkectl=$(nvkectl version 2>/dev/null | head -n1 | tr -d '\n')"
		echo "sections: $(awk -F'\t' 'NR>1{n++; if($2=="0")ok++; else if($2=="-")sk++; else f++} END{printf "%d ok=%d failed=%d skipped=%d", n, ok, f, sk}' "$INDEX")"
		echo "bytes: $(du -sk "$OUT" 2>/dev/null | cut -f1)K"
	} >"$MANIFEST"
	# A command cut off by the deadline may still be writing when tar reads
	# its file, which tar reports as status 1; the archive is still complete
	# for every finished section, so it is kept whenever it was written.
	tar -czf "$ARCHIVE.partial" -C "${OUT%/*}" "${OUT##*/}" 2>/dev/null
	if [ -s "$ARCHIVE.partial" ]; then
		mv -f "$ARCHIVE.partial" "$ARCHIVE"
	fi
	exit "$status"
}
trap finish EXIT
trap 'INTERRUPTED=signal; exit 124' TERM INT HUP

# The sampler's timeline: which network each pod landed on, and when. Local
# and cheap, so it goes first; the sampler rotates at 8 MiB and keeps one
# older file, and the copy keeps the tail of each.
copydir timeline $((8 * M)) "$TIMELINE_DIR/timeline" 4

# ---------------------------------------------------------------- cluster ---
# Cheapest and highest-value first, so a deadline still leaves the evidence
# that decides a failure.

if timeout 15 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=10s get --raw /readyz >/dev/null 2>&1; then
	API_REACHABLE=yes
else
	API_REACHABLE=no
	KUBECTL_TIMEOUT=8
fi

# The DNS group: the kube-dns Service, its endpoints, the pods behind it, and
# their network placement. This is what settled the first bundle-less failure.
kube cluster/dns/kube-dns.yaml 30 $((1 * M)) -n kube-system get service,endpoints,endpointslices -l k8s-app=kube-dns -o yaml
kube cluster/dns/coredns-configmap.yaml 30 $((1 * M)) -n kube-system get configmap coredns -o yaml
kube cluster/dns/dns-pods.txt 30 $((1 * M)) -n kube-system get pods -l 'k8s-app in (kube-dns,node-local-dns)' -o wide
kube cluster/pods-wide.txt 30 $((2 * M)) get pods -A -o wide
kube cluster/nodes.yaml 30 $((2 * M)) get nodes -o yaml
kube cluster/nodes.describe.txt 30 $((2 * M)) describe nodes
kube cluster/events.txt 30 $((4 * M)) get events -A --sort-by=.lastTimestamp -o wide
kube cluster/services.txt 30 $((2 * M)) get services -A -o wide
kube cluster/endpointslices.yaml 30 $((4 * M)) get endpointslices -A -o yaml
kube cluster/workloads.txt 30 $((2 * M)) get deployments,daemonsets,statefulsets,jobs -A -o wide
# The controller's namespace, which is what the tenant specs wait on.
kube cluster/controller/all.txt 30 $((1 * M)) -n "$CONTROLLER_NAMESPACE" get all,endpoints,endpointslices -o wide
kube cluster/controller/pods.describe.txt 30 $((4 * M)) -n "$CONTROLLER_NAMESPACE" describe pods
kube cluster/controller/logs.txt 60 $((4 * M)) -n "$CONTROLLER_NAMESPACE" logs -l app.kubernetes.io/name --all-containers --tail=2000 --prefix --ignore-errors
kube cluster/controller/logs.previous.txt 60 $((4 * M)) -n "$CONTROLLER_NAMESPACE" logs -l app.kubernetes.io/name --all-containers --tail=2000 --prefix --ignore-errors --previous

if [ "$API_REACHABLE" = yes ]; then
	kube cluster/pods.describe.txt 60 $((8 * M)) describe pods -A
	kube cluster/workloads.describe.txt 60 $((8 * M)) describe deployments,daemonsets,statefulsets,jobs -A
	# ConfigMaps carry the kubeadm, kubelet, CoreDNS and OVN configuration. The
	# suite scrubs them before upload; Secrets are never read.
	kube cluster/configmaps.yaml 60 $((8 * M)) get configmaps -A -o yaml
	kube cluster/crds.txt 30 $((1 * M)) get crds
	kube cluster/apiservices.txt 30 $((1 * M)) get apiservices
	kube cluster/webhooks.yaml 30 $((2 * M)) get mutatingwebhookconfigurations,validatingwebhookconfigurations -o yaml
	kube cluster/leases.txt 30 $((1 * M)) get leases -A
	kube cluster/storage.txt 30 $((1 * M)) get storageclasses,persistentvolumes,persistentvolumeclaims -A
	kube cluster/networkpolicies.txt 30 $((1 * M)) get networkpolicies -A
	kube cluster/readyz.txt 15 $((64 * K)) get --raw '/readyz?verbose'
	kube cluster/livez.txt 15 $((64 * K)) get --raw '/livez?verbose'
	kube cluster/version.txt 15 $((64 * K)) version

	# The Cluster API and provider objects, whatever the cluster has of them.
	timeout 30 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=20s api-resources --verbs=list -o name 2>/dev/null |
		grep -E 'cluster\.x-k8s\.io|nvke|nico|provisioned' | head -n 25 >"$TMP/capi-resources" || true
	while IFS= read -r resource; do
		[ -n "$resource" ] || continue
		kube "cluster/capi/$resource.yaml" 30 $((4 * M)) get "$resource" -A -o yaml
		kube "cluster/capi/$resource.describe.txt" 30 $((4 * M)) describe "$resource" -A
	done <"$TMP/capi-resources"

	# The previous log of every container that restarted, which is the only
	# copy of why a crash-looping container died. Init containers too: a
	# schema job stuck in Init is one of these.
	timeout 30 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=20s get pods -A -o go-template='{{range .items}}{{$ns := .metadata.namespace}}{{$pod := .metadata.name}}{{range .status.initContainerStatuses}}{{if gt .restartCount 0}}{{$ns}} {{$pod}} {{.name}}{{"\n"}}{{end}}{{end}}{{range .status.containerStatuses}}{{if gt .restartCount 0}}{{$ns}} {{$pod}} {{.name}}{{"\n"}}{{end}}{{end}}{{end}}' 2>/dev/null |
		head -n 60 >"$TMP/restarted" || true
	while read -r ns pod container; do
		[ -n "$container" ] || continue
		kube "cluster/pods.previous-logs/${ns}_${pod}_${container}.log" 10 $((512 * K)) -n "$ns" logs "$pod" -c "$container" --previous --tail=2000 --ignore-errors
	done <"$TMP/restarted"

	# Releases and their history. Not values or manifests: those can carry
	# credentials, and the objects they rendered are in the cluster dump.
	run cluster/helm-list.yaml 30 $((1 * M)) env KUBECONFIG="$KUBECONFIG_PATH" helm list -A -o yaml
	timeout 30 env KUBECONFIG="$KUBECONFIG_PATH" helm list -A --no-headers 2>/dev/null | awk '{print $2, $1}' | head -n 20 >"$TMP/releases" || true
	while read -r ns release; do
		[ -n "$release" ] || continue
		run "cluster/helm-history/${ns}_${release}.txt" 20 $((256 * K)) env KUBECONFIG="$KUBECONFIG_PATH" helm history "$release" -n "$ns"
	done <"$TMP/releases"

	# OVN's own view of services and chassis, from inside the ovnkube-node pod.
	ovn_pod=$(timeout 20 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=10s -n ovn-kubernetes get pods -l name=ovnkube-node -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	if [ -n "$ovn_pod" ]; then
		kube cluster/ovn/nbctl-show.txt 30 $((1 * M)) -n ovn-kubernetes exec "$ovn_pod" -c nb-ovsdb -- ovn-nbctl --timeout=10 show
		kube cluster/ovn/nbctl-lb-list.txt 30 $((1 * M)) -n ovn-kubernetes exec "$ovn_pod" -c nb-ovsdb -- ovn-nbctl --timeout=10 lb-list
		kube cluster/ovn/sbctl-show.txt 30 $((1 * M)) -n ovn-kubernetes exec "$ovn_pod" -c sb-ovsdb -- ovn-sbctl --timeout=10 show
		kube cluster/ovn/sbctl-chassis.txt 30 $((1 * M)) -n ovn-kubernetes exec "$ovn_pod" -c sb-ovsdb -- ovn-sbctl --timeout=10 list chassis
	else
		skip cluster/ovn "no ovnkube-node pod"
	fi

	# Last of the cluster group and the broadest: the current log of every
	# container, init containers included, each through kube's cap, at most
	# this many of them and for at most this long. Not `cluster-info dump`:
	# its --output-directory writes past every cap this script has.
	timeout 30 kubectl --kubeconfig "$KUBECONFIG_PATH" --request-timeout=20s get pods -A -o go-template='{{range .items}}{{$ns := .metadata.namespace}}{{$pod := .metadata.name}}{{range .spec.initContainers}}{{$ns}} {{$pod}} {{.name}}{{"\n"}}{{end}}{{range .spec.containers}}{{$ns}} {{$pod}} {{.name}}{{"\n"}}{{end}}{{end}}' 2>/dev/null |
		head -n 120 >"$TMP/containers" || true
	logs_t0=$(date +%s)
	while read -r ns pod container; do
		[ -n "$container" ] || continue
		if [ $(($(date +%s) - logs_t0)) -ge 150 ]; then
			skip cluster/pods.logs/... "capped at 150 seconds"
			break
		fi
		kube "cluster/pods.logs/${ns}_${pod}_${container}.log" 10 $((512 * K)) -n "$ns" logs "$pod" -c "$container" --tail=2000 --ignore-errors
	done <"$TMP/containers"
else
	skip cluster/heavy "API server not reachable: describe, configmaps, CAPI objects, previous logs, helm, OVN and pod logs not attempted"
fi

# ------------------------------------------------------------------- node ---

# The CNI configuration kubelet reads, in order, and the plugins it can run.
run node/cni/net.d.listing.txt 10 $((64 * K)) ls -la "${HOST_ROOT}/etc/cni/net.d"
copydir node/cni/net.d $((256 * K)) /etc/cni/net.d
run node/cni/opt-cni-bin.txt 10 $((64 * K)) ls -la "${HOST_ROOT}/opt/cni/bin"

run node/net/ip-addr.txt 10 $((1 * M)) ip -d addr
run node/net/ip-link.txt 10 $((1 * M)) ip -d link
run node/net/ip-route-all.txt 10 $((1 * M)) ip route show table all
run node/net/ip-rule.txt 10 $((256 * K)) ip rule
run node/net/ip6-route.txt 10 $((1 * M)) ip -6 route show table all
run node/net/ip-neigh.txt 10 $((1 * M)) ip neigh
run node/net/bridge-link.txt 10 $((1 * M)) bridge link
run node/net/nft-ruleset.txt 20 $((2 * M)) nft list ruleset
run node/net/iptables-save.txt 20 $((2 * M)) iptables-save
run node/net/ss-listen.txt 10 $((1 * M)) ss -lntup
run node/net/ss-summary.txt 10 $((256 * K)) ss -s
copy node/net/resolv.conf $((64 * K)) /etc/resolv.conf
run node/net/resolvectl.txt 10 $((256 * K)) resolvectl status
copy node/net/hosts $((64 * K)) /etc/hosts
copydir node/net/netplan $((64 * K)) /etc/netplan
run node/net/sysctl.txt 10 $((512 * K)) sh -c 'sysctl -a 2>/dev/null | grep -E "^(net\.(ipv4|ipv6|bridge|core)|fs\.inotify)"'
run node/net/lsmod.txt 10 $((256 * K)) lsmod

run node/ovs/vsctl-show.txt 20 $((1 * M)) ovs-vsctl --timeout=10 show
run node/ovs/vsctl-open_vswitch.txt 20 $((1 * M)) ovs-vsctl --timeout=10 list Open_vSwitch
run node/ovs/ofctl-ports-br-int.txt 20 $((1 * M)) ovs-ofctl dump-ports-desc br-int
run node/ovs/ofctl-ports-br-ex.txt 20 $((1 * M)) ovs-ofctl dump-ports-desc br-ex
run node/ovs/dpif-show.txt 20 $((1 * M)) ovs-appctl -t ovs-vswitchd dpif/show
run node/ovs/flows-br-int.txt 30 $((4 * M)) ovs-ofctl dump-flows br-int
run node/ovs/dpctl-flows.txt 30 $((2 * M)) ovs-appctl -t ovs-vswitchd dpctl/dump-flows

copydir node/logs/ovn-kubernetes $((4 * M)) /var/log/ovn-kubernetes
copydir node/logs/openvswitch $((2 * M)) /var/log/openvswitch
copydir node/logs/ovn $((2 * M)) /var/log/ovn
copy node/logs/cloud-init.log $((2 * M)) /var/log/cloud-init.log
copy node/logs/cloud-init-output.log $((2 * M)) /var/log/cloud-init-output.log
run node/logs/cloud-init-status.txt 20 $((64 * K)) cloud-init status --long
run node/logs/postinst-listing.txt 10 $((64 * K)) sh -c 'ls -la "$1"/nke-*-postinst.log "$1"/nvke-*-postinst.log 2>&1' _ "${HOST_ROOT}/var/log"
for f in "${HOST_ROOT}"/var/log/nke-*-postinst.log "${HOST_ROOT}"/var/log/nvke-*-postinst.log; do
	[ -f "$f" ] || continue
	copy "node/logs/postinst/$(basename "$f")" $((2 * M)) "${f#"$HOST_ROOT"}"
done
copy node/logs/nvke-agent-audit.log $((2 * M)) /var/log/nvke-agent-audit.log
# Script arguments and output; scrubbed by the suite before upload.
copy node/logs/nvke-execute-scripts.log $((2 * M)) /var/log/nvke-execute-scripts.log
# The deploy path's own state and diagnostics, written under root's HOME.
# nvke-deploy.sh renders the deploy path into renders/ there, and a rendered
# item carries the site's credentials in its values, so renders/ is pruned
# whole, along with every directory and name the suite's deny list refuses.
# Pruned before the cap, so a denied file does not take one of the 20 slots,
# and each copy keeps its relative path, so the deny list reads the same
# names it would on the node.
if [ -d "${HOST_ROOT}/root/.local/state/nvke" ]; then
	run node/logs/deploy-state.listing.txt 10 $((256 * K)) find "${HOST_ROOT}/root/.local/state/nvke" -maxdepth 3 -ls
	find "${HOST_ROOT}/root/.local/state/nvke" \( -type d \( -name renders -o -name values -o -name pki -o -name teleport -o -name .tsh \) -prune \) -o \
		-type f ! -name '*kubeconfig*' ! -name 'admin.conf' ! -name 'super-admin.conf' ! -name 'controller-manager.conf' ! -name 'scheduler.conf' ! -name 'kubelet.conf' \
		! -name 'sitespec.yaml' ! -name 'spire.env' ! -name 'spire-agent.env' ! -name '*.key' ! -name '*.pem' -print 2>/dev/null | head -n 20 >"$TMP/deploy-state" || true
	while IFS= read -r f; do
		[ -n "$f" ] || continue
		copy "node/logs/deploy-state/${f#"${HOST_ROOT}/root/.local/state/nvke/"}" $((2 * M)) "${f#"$HOST_ROOT"}"
	done <"$TMP/deploy-state"
else
	skip node/logs/deploy-state "no /root/.local/state/nvke"
fi

run node/journal/boots.txt 20 $((256 * K)) journalctl --list-boots --no-pager
run node/journal/warnings.txt 30 $((4 * M)) journalctl -b -p warning -n 20000 -o short-iso --no-pager
for unit in nke-agent nvke-agent spire-agent nvke-spire-configure kubelet containerd openvswitch-switch ovsdb-server ovs-vswitchd ovn-controller buildkit cloud-init cloud-init-local cloud-config cloud-final; do
	run "node/journal/units/$unit.log" 30 $((4 * M)) journalctl -b -u "$unit" -n 20000 -o short-iso --no-pager
done
run node/systemd/failed.txt 10 $((256 * K)) systemctl --failed --no-pager
run node/systemd/units.txt 10 $((1 * M)) systemctl list-units --all --type=service --no-pager
for unit in nke-agent spire-agent kubelet containerd openvswitch-switch; do
	run "node/systemd/status-$unit.txt" 10 $((256 * K)) systemctl status "$unit" --no-pager -l
done

run node/runtime/crictl-ps.txt 20 $((1 * M)) crictl --timeout 10s ps -a
run node/runtime/crictl-pods.txt 20 $((1 * M)) crictl --timeout 10s pods
run node/runtime/crictl-images.txt 20 $((1 * M)) crictl --timeout 10s images
run node/runtime/crictl-info.json 20 $((1 * M)) crictl --timeout 10s info
# Container logs straight from the runtime, for when the API server cannot
# serve them: the static control plane and the CNI.
crictl_n=0
for ns in kube-system ovn-kubernetes; do
	timeout 20 crictl --timeout 10s ps -a -q --label "io.kubernetes.pod.namespace=$ns" 2>/dev/null | head -n 40 >"$TMP/containers-$ns" || true
	while IFS= read -r id; do
		[ -n "$id" ] || continue
		crictl_n=$((crictl_n + 1))
		if [ "$crictl_n" -gt 40 ]; then
			skip node/runtime/crictl-logs/... "capped at 40 containers"
			break 2
		fi
		label=$(timeout 10 crictl --timeout 5s inspect -o go-template --template '{{index .status.labels "io.kubernetes.pod.name"}}_{{index .status.labels "io.kubernetes.container.name"}}' "$id" 2>/dev/null || echo "$id")
		run "node/runtime/crictl-logs/${ns}_${label}.log" 10 $((256 * K)) crictl --timeout 5s logs --tail 500 "$id"
	done <"$TMP/containers-$ns"
done
run node/runtime/ctr-images.txt 20 $((1 * M)) ctr -n k8s.io images ls

# What the kind seed left behind: nerdctl networks and containers, and the
# seed node's own runtime, which is the only evidence when the bootstrap
# fails before the site kubeconfig exists.
run node/seed/nerdctl-ps.txt 20 $((256 * K)) nerdctl ps -a
run node/seed/nerdctl-networks.txt 20 $((256 * K)) nerdctl network ls
timeout 20 nerdctl network ls --format '{{.Name}}' 2>/dev/null | head -n 10 >"$TMP/networks" || true
while IFS= read -r net; do
	[ -n "$net" ] || continue
	run "node/seed/nerdctl-network-$net.json" 20 $((256 * K)) nerdctl network inspect "$net"
done <"$TMP/networks"
run node/seed/kind-clusters.txt 20 $((64 * K)) env KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters
timeout 20 nerdctl ps -a --format '{{.Names}}' 2>/dev/null | grep -E -- '-control-plane$|-worker[0-9]*$' | head -n 5 >"$TMP/seed-nodes" || true
while IFS= read -r c; do
	[ -n "$c" ] || continue
	run "node/seed/$c-crictl-ps.txt" 30 $((1 * M)) nerdctl exec "$c" crictl ps -a
	run "node/seed/$c-kubelet.journal" 30 $((1 * M)) nerdctl exec "$c" journalctl -u kubelet -n 2000 --no-pager
done <"$TMP/seed-nodes"

run node/packages/dpkg.txt 20 $((1 * M)) sh -c 'dpkg -l 2>/dev/null | grep -Ei "nvke|nke-|containerd|kube|openvswitch|ovn|spire|nerdctl"'
run node/packages/apt-policy.txt 30 $((1 * M)) sh -c 'apt-cache policy $(dpkg-query -W -f "\${Package}\n" 2>/dev/null | grep -E "^nvke-" | head -n 40) 2>&1'
copydir node/packages/sources $((64 * K)) /etc/apt/sources.list.d
run node/packages/nvke-debs.txt 10 $((256 * K)) ls -la "${HOST_ROOT}/usr/lib/nvke/debs"

copydir node/config/manifests $((256 * K)) /etc/kubernetes/manifests
copy node/config/kubelet-config.yaml $((256 * K)) /var/lib/kubelet/config.yaml
copy node/config/kubeadm-flags.env $((64 * K)) /var/lib/kubelet/kubeadm-flags.env
# Registry credentials stay on the node: config.toml's auth block and
# hosts.toml's Authorization header lose their values here, and the suite's
# scrub runs over the result again.
run node/config/containerd-config.toml 10 $((256 * K)) sh -c 'sed -E "s/^([[:space:]]*(auth|identitytoken|password|username)[[:space:]]*=[[:space:]]*).*/\1[stripped]/" "$1" 2>&1' _ "${HOST_ROOT}/etc/containerd/config.toml"
run node/config/containerd-certs.d.txt 10 $((256 * K)) sh -c 'for f in "$1"/etc/containerd/certs.d/*/hosts.toml; do [ -f "$f" ] || continue; echo "== $f"; sed -E "s/([Aa]uthorization\"?[[:space:]]*=[[:space:]]*)[^}]*/\1[stripped]/g" "$f"; done' _ "$HOST_ROOT"
copy node/config/crictl.yaml $((64 * K)) /etc/crictl.yaml
copy node/config/default/nke-agent $((64 * K)) /etc/default/nke-agent
copy node/config/default/nvke-agent $((64 * K)) /etc/default/nvke-agent
copy node/config/spire-agent.conf $((256 * K)) /etc/spire-agent/agent.conf
# The agent's state carries kubeadm join strings. Stripped here, and the
# suite's scrub runs over it again.
run node/config/nvke-state.json 10 $((1 * M)) sh -c 'sed -E "s/(\"(controlPlaneJoinString|workerJoinString)\"[[:space:]]*:[[:space:]]*\")[^\"]*\"/\1[stripped]\"/g" "$1" 2>&1' _ "${HOST_ROOT}/var/lib/nvke/state.json"

run node/system/uname.txt 10 $((64 * K)) uname -a
copy node/system/os-release $((64 * K)) /etc/os-release
run node/system/uptime.txt 10 $((64 * K)) uptime
run node/system/date.txt 10 $((64 * K)) date -u
run node/system/timedatectl.txt 10 $((64 * K)) timedatectl
run node/system/free.txt 10 $((64 * K)) free -m
run node/system/df.txt 10 $((256 * K)) df -h
run node/system/lsblk.txt 10 $((256 * K)) lsblk
run node/system/mount.txt 10 $((512 * K)) sh -c 'mount | grep -E "kubelet|containerd|cni|nke|/ |/var"'
run node/system/dmesg.txt 20 $((1 * M)) sh -c 'dmesg -T 2>/dev/null | tail -n 5000'
run node/system/top.txt 20 $((256 * K)) sh -c 'top -bn1 2>/dev/null | head -n 40'
run node/nke-e2e.listing.txt 10 $((256 * K)) ls -la "${HOST_ROOT}/var/lib/nke-e2e"
