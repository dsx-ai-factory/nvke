#!/usr/bin/env bash
# Renders the recipe in data/ to Argo CD Applications.
#
#   ./render.sh [output-dir]
#
# Swap the leaf overlay to compare variants:
#   data/overlays/forge.yaml          full stack
#   data/overlays/forge-coexist.yaml  controller layer only
set -euo pipefail
cd "$(dirname "$0")"
aicr bundle \
  --recipe data/overlays/forge.yaml \
  --data data \
  --deployer argocd \
  --repo https://github.com/NVIDIA/nke \
  --output "${1:-./generated}"
