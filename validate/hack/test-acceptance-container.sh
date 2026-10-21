#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Real-image acceptance run, zero egress: validates a real NVKE node image
# through every tier inside a --network none container, signs a verdict, and
# re-checks it under the full qualification profile — plus the negative cases
# (tampered bytes, wrong target, omitted tiers, wrong digest).
#
# Two phases because an isolated container cannot install anything:
#   1. build a tools image (network ON, once; rebuilt only when BUILD_TOOLS=1)
#   2. run acceptance (--network none --privileged)
#
# Inputs (paths must be mountable by your Docker daemon; VM-backed daemons
# typically share only $HOME):
#   VALIDATE_IMAGE  real qcow2 (e.g. nke-node-fde-ubuntu-24.04.5-arm64.qcow2)
#   ACCEPT_BUNDLE   evidence dir: checksums.txt, its sigstore bundle, the
#                   pinned trusted root, and this artifact's *.spdx.json
#   ACCEPT_BIN      dir with linux/$(arch) nvke-image-validate + nvke-verdict
#   ACCEPT_CHECKS   checks.yaml to qualify against (defaults to the repo's)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${VALIDATE_IMAGE:?set VALIDATE_IMAGE to the real qcow2}"
: "${ACCEPT_BUNDLE:?set ACCEPT_BUNDLE to the evidence bundle dir}"
: "${ACCEPT_BIN:?set ACCEPT_BIN to the dir holding linux binaries}"
ACCEPT_CHECKS="${ACCEPT_CHECKS:-${REPO_ROOT}/validate/checks.yaml}"
TOOLS_TAG=nvke-acceptance-tools

if [ "${BUILD_TOOLS:-0}" = "1" ] || ! docker image inspect "${TOOLS_TAG}" >/dev/null 2>&1; then
  docker build -t "${TOOLS_TAG}" -f - "${REPO_ROOT}/validate/hack" <<'EOF'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -q && apt-get install -yq \
      libguestfs-tools linux-image-virtual \
      qemu-system-arm qemu-system-x86 qemu-efi-aarch64 ovmf \
      cloud-image-utils genisoimage openssh-client openssl python3 \
    && chmod 644 /boot/vmlinuz* \
    && rm -rf /var/lib/apt/lists/*
EOF
fi

ARTIFACT="$(basename "${VALIDATE_IMAGE}")"
ACCEPT_OUT="${ACCEPT_OUT:-${ACCEPT_BUNDLE}/evidence-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "${ACCEPT_OUT}"
docker run --rm -i --privileged --network none \
  -v "${VALIDATE_IMAGE}:/in/${ARTIFACT}:ro" \
  -v "${ACCEPT_BUNDLE}:/in/bundle:ro" \
  -v "${ACCEPT_BIN}:/in/bin:ro" \
  -v "${ACCEPT_CHECKS}:/in/checks.yaml:ro" \
  -v "${ACCEPT_OUT}:/out" \
  -e "ARTIFACT=${ARTIFACT}" \
  "${TOOLS_TAG}" bash -euo pipefail <<'EOF'
export LIBGUESTFS_BACKEND=direct
VALIDATE=/in/bin/nvke-image-validate
VERDICT=/in/bin/nvke-verdict
W=/tmp/accept && mkdir -p $W && cd $W

echo "=== zero-egress evidence: no default route, no global addresses, no resolution ==="
# The observation tools must exist and succeed: a missing `ip` must read as
# "cannot observe", never as "isolated".
command -v ip >/dev/null || { echo "FATAL: ip is not available; cannot assert isolation"; exit 1; }
command -v getent >/dev/null || { echo "FATAL: getent is not available; cannot assert isolation"; exit 1; }
ROUTES=$(ip route) || { echo "FATAL: ip route failed"; exit 1; }
echo "$ROUTES" | grep -q '^default' && { echo "FATAL: container has a default route"; exit 1; }
ADDRS=$(ip -o addr show scope global | wc -l) || { echo "FATAL: ip addr failed"; exit 1; }
[ "$ADDRS" -eq 0 ] || { echo "FATAL: $ADDRS global addresses present"; exit 1; }
getent hosts github.com >/dev/null 2>&1 && { echo "FATAL: name resolution works"; exit 1; }
echo "   isolated: asserted (no default route, 0 global addresses, no resolution)"
{ echo "qemu: $(qemu-system-aarch64 --version | head -1)"; dpkg-query -W libguestfs-tools qemu-system-arm 2>/dev/null; sha256sum /in/bin/* /in/checks.yaml; } > /out/environment.txt

# Target identity from the artifact name: nke/nvke-node-<variant>-ubuntu-<ver>-<arch>.qcow2
VARIANT=$(echo "$ARTIFACT" | sed -E 's/^n?vke?-node-(fde|non-fde)-.*/\1/; s/^nke-node-(fde|non-fde)-.*/\1/')
UBUNTU=$(echo "$ARTIFACT" | sed -E 's/.*ubuntu-([0-9][0-9.]*)-.*/\1/')
ARCH=$(echo "$ARTIFACT" | sed -E 's/.*-(amd64|arm64)\.qcow2$/\1/')
TARGET=(--target-variant "$VARIANT" --target-arch "$ARCH" --target-ubuntu "$UBUNTU")
echo "   target: $VARIANT/$ARCH/ubuntu-$UBUNTU"

# Target BOM from the attested SBOM: the NVKE components and their versions.
python3 - "/in/bundle/${ARTIFACT%.qcow2}.spdx.json" > bom.yaml <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
print("components:")
for p in doc.get("packages", []):
    if p.get("name") in ("nke-agent", "nvke-agent", "spire-agent"):
        print(f"  {p['name']}: {p['versionInfo']}")
PY
cat bom.yaml

printf 'changeit' > luks.pass
openssl ecparam -genkey -name prime256v1 -noout | openssl pkcs8 -topk8 -nocrypt -out sign.key
openssl ec -in sign.key -pubout -out verdict.pub 2>/dev/null
CHECKSET_SHA=$(sha256sum /in/checks.yaml | cut -d' ' -f1)
ALL_CHECKS=$(python3 -c "import re;print(','.join(re.findall(r'^  - id: (\S+)', open('/in/checks.yaml').read(), re.M)))")
echo "   profile: $ALL_CHECKS"

# run's own output goes to stderr so a redirected command's stdout (a JSON
# report) stays a single clean document.
run() { local want=$1; shift; local got=0; "$@" || got=$?; [ "$got" = "$want" ] || { echo "FATAL: expected exit $want got $got: $*" >&2; exit 1; }; echo "   => exit $got (expected $want)" >&2; }

echo "=== POSITIVE: full-tier validation of the real image ==="
time run 0 $VALIDATE --image "/in/$ARTIFACT" --name "$ARTIFACT" --bundle /in/bundle --trusted-root /in/bundle/trusted-root.json \
  --checks /in/checks.yaml "${TARGET[@]}" \
  --inspect on --boot on --boot-timeout 30m --luks-passphrase-file luks.pass \
  --target-bom bom.yaml --emit-verdict verdict.json --sign-key sign.key \
  --format json > /out/report.json
WANT=$(( $(echo "$ALL_CHECKS" | tr ',' '\n' | wc -l) ))
python3 - /out/report.json "$WANT" <<'PY'
import json, sys
rep = json.load(open(sys.argv[1])); want = int(sys.argv[2]); s = rep["summary"]
assert s["pass"] == want and s["skip"] == 0 and s["error"] == 0 and s["fail"] == 0, s
print(f"   asserted: pass={s['pass']} skip=0 fail=0 error=0 (profile size {want})")
PY
cp verdict.json verdict.pub bom.yaml /out/

echo "=== POSITIVE: verdict passes the full qualification profile ==="
run 0 $VERDICT --attestation verdict.json --key verdict.pub --image "/in/$ARTIFACT" \
  --required-checks "$ALL_CHECKS" --check-set-sha256 "$CHECKSET_SHA" \
  --target-variant "$VARIANT" --target-arch "$ARCH" --target-ubuntu "$UBUNTU"

echo "=== NEGATIVE: tampered bytes fail INT-001, named ==="
cp "/in/$ARTIFACT" tampered.qcow2 && printf 'X' >> tampered.qcow2
run 1 $VALIDATE --image tampered.qcow2 --name "$ARTIFACT" --bundle /in/bundle --trusted-root /in/bundle/trusted-root.json \
  --checks /in/checks.yaml "${TARGET[@]}" --inspect off --format json > /out/tampered-report.json
python3 -c 'import json,sys; r=[x for x in json.load(open("/out/tampered-report.json"))["results"] if x["id"]=="INT-001"][0]; assert r["status"]=="fail", r; print("   asserted: INT-001 fail named")' 

echo "=== NEGATIVE: wrong target fails COMPAT-001 ==="
WRONG=amd64; [ "$ARCH" = amd64 ] && WRONG=arm64
run 1 $VALIDATE --image "/in/$ARTIFACT" --name "$ARTIFACT" --bundle /in/bundle --trusted-root /in/bundle/trusted-root.json \
  --checks /in/checks.yaml --target-variant "$VARIANT" --target-arch "$WRONG" \
  --target-ubuntu "$UBUNTU" --inspect off --format json > /out/wrong-target-report.json
python3 -c 'import json; r=[x for x in json.load(open("/out/wrong-target-report.json"))["results"] if x["id"]=="COMPAT-001"][0]; assert r["status"]=="fail", r; print("   asserted: COMPAT-001 fail named")' 

echo "=== NEGATIVE: an omitted-tier verdict cannot satisfy the profile ==="
run 0 $VALIDATE --image "/in/$ARTIFACT" --name "$ARTIFACT" --bundle /in/bundle --trusted-root /in/bundle/trusted-root.json \
  --checks /in/checks.yaml "${TARGET[@]}" --inspect off \
  --emit-verdict lazy.json --sign-key sign.key >/dev/null
run 2 $VERDICT --attestation lazy.json --key verdict.pub --image "/in/$ARTIFACT" \
  --required-checks "$ALL_CHECKS" --check-set-sha256 "$CHECKSET_SHA"

echo "=== NEGATIVE: the verdict does not transfer to other bytes ==="
run 2 $VERDICT --attestation verdict.json --key verdict.pub --image tampered.qcow2 \
  --required-checks "$ALL_CHECKS"

echo "ACCEPTANCE RUN COMPLETE: all positive and negative cases held, zero egress."
EOF
