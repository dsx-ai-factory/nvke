#!/bin/sh
set -eu
counts="$(kubectl --kubeconfig /kubeconfig/value get pods -n kube-system \
  -l component=kube-apiserver \
  -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.containerStatuses[?(@.name=="kube-apiserver")].restartCount}{"\n"}{end}' \
  | sort)"
if [ -z "$counts" ]; then
  echo "no kube-apiserver restart counts found" > /dev/termination-log
  exit 1
fi
printf '%s' "$counts" > /dev/termination-log
