# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Canonical tool versions. This file contains values only so module Makefiles
# can include it without adding root targets to their help output.

# Derived, not duplicated: go.work owns the Go version.
GOVULNCHECK_GO_VERSION ?= $(shell awk '/^go /{print $$2; exit}' $(dir $(lastword $(MAKEFILE_LIST)))../go.work)
GOVULNCHECK_VERSION    ?= v1.6.0
# Waived govulncheck findings, as <module>=<OSV-id>[,<OSV-id>...]. Scoped to
# the advisory, not the module, so an unrelated reachable vulnerability in the
# same module still fails the gate. Empty: nothing is waived. A waived advisory
# that stops being called fails the gate, so an entry cannot outlive its reason.
GOVULNCHECK_WAIVED ?=
# Set both with any waiver. Name a person as the owner, not a team.
GOVULNCHECK_WAIVER_OWNER  ?=
GOVULNCHECK_WAIVER_REVIEW ?=

# Committed digests, not fetched from the release. A checksum served by the
# same host as the artifact shares its compromise boundary; a committed one
# does not. One line per tool per platform, updated with the version above it.
ACTIONLINT_VERSION ?= 1.7.12
ACTIONLINT_SHA256_AMD64 ?= 8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
SHELLCHECK_VERSION ?= 0.11.0
# Next to the version so a bump cannot change one without the other.
# linux.x86_64 tarball for the version above.
SHELLCHECK_SHA256  ?= 8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
GITLEAKS_VERSION   ?= 8.30.1
GITLEAKS_SHA256_AMD64 ?= 551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb
GITLEAKS_SHA256_ARM64 ?= e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080

# The GitHub CLI, for `gh attestation verify` in the image promotion gates.
# Pinned rather than taken from the runner image: the promotion jobs run on
# self-hosted runners, where gh may be absent or old, and the flags those gates
# depend on -- --signer-workflow, --source-digest, --source-ref -- are not in
# every release. A gate that silently degrades is not a gate.
GH_CLI_VERSION ?= 2.97.0
GH_CLI_SHA256_AMD64 ?= a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112
GH_CLI_SHA256_ARM64 ?= 73ea440ecad9c9e284429997ee6f93577bc6f7bc6fba357ef62c53ad8fb641a5

# tsh and tbot for the Forge site-controller lane, out of one tarball. This
# pin is not free to move: it is matched to the Teleport cluster's server
# version rather than merely kept within the supported skew, because an
# unpinned client against a pinned cluster surfaces as an unexplained relay
# error deep in a 40-minute run. Bump it when the cluster is upgraded.
# teleport-v<version>-linux-amd64-bin.tar.gz.
TELEPORT_VERSION   ?= 18.11.0
TELEPORT_SHA256_AMD64 ?= 8d3dba03ea6b6e0d72c97e190da0595e8f0d70f5573c3a26b1908cf60caf6e3f

KUBECONFORM_VERSION ?= v0.7.0
# .github/actions/setup-kubeconform installs it for chart.yml and nktp-chart.yml,
# both on ubuntu-latest, so amd64 is the only platform needed.
KUBECONFORM_SHA256_AMD64 ?= c31518ddd122663b3f3aa874cfe8178cb0988de944f29c74a0b9260920d115d3
HELM_VERSION         ?= v4.2.4

# crane and nfpm build the CNI debs under os-image/packages/.
CRANE_VERSION ?= 0.21.9
CRANE_SHA256_AMD64 ?= 5c16d8ddb971cb1d5e6ed8b1e743da8224414eeba2c2762d8f1a61b2f095699e
CRANE_SHA256_ARM64 ?= 1f4c647b7bb260ab5435661df5b526cf59950ebf95201790db7183ac189cbcbd
NFPM_VERSION  ?= 2.47.0

# The Ubuntu Server installer ISO, one line per point release per architecture.
# os-image/packer/Makefile selects by UBUNTU_RELEASE and ARCH and fails closed
# on a combination with no line here, so a release bump lands with its digest.
#
# Committed for exactly the reason stated at the top of this file. ISO_CHECKSUM
# used to be file:<the mirror's own SHA256SUMS>, which is served by the same
# host as the ISO: a host that substitutes the image serves the matching
# checksum with it, so that form cannot reject one.
#
# Taken from Canonical's SHA256SUMS and verified against the SHA256SUMS.gpg
# beside it with the Ubuntu CD Image Automatic Signing Key (2012),
# 8439 38DF 228D 22F7 B374  2BC0 D94A A3F0 EFE2 1092. To add or re-check a line:
#
#   gpg --verify SHA256SUMS.gpg SHA256SUMS && grep live-server SHA256SUMS
#
# amd64 is published under releases.ubuntu.com/<series>/, arm64 only under
# cdimage.ubuntu.com/ubuntu/releases/<series>/release/ -- releases.ubuntu.com
# has no arm64 entry at all.
UBUNTU_ISO_SHA256_24_04_5_AMD64 ?= 97f3d7ffb032c3eb3b23d2c8be9cc76e60c2c1f2c0146ba5ba9fe01cafae0fd8
UBUNTU_ISO_SHA256_24_04_5_ARM64 ?= f12484464c7d4d73e2167e0a7dbda1e669f8fae9bcf778dc0944937034b320cb
UBUNTU_ISO_SHA256_26_04_1_AMD64 ?= cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927
UBUNTU_ISO_SHA256_26_04_1_ARM64 ?= af78753c94b924c85c4964c670692a853f7c80299a4064e317edad95462e3168

# The git ref whose controller chart DEFAULTS still render the legacy
# (pre-nvke-rename) names: release cloud-workflow-engine, namespace nke-system,
# ConfigMap nke-controller-config. chart-upgrade-identity.sh exports the chart
# at this ref, installs it, and upgrades it with the pinned values a site sets,
# to prove the pins hold against a real API server.
#
# Not a version to keep current -- the opposite. It names the last published
# chart a site could be running from BEFORE the rename, and it moves only when
# that stops being true (#1496). The defaults moved in #1698, #1769 and #1818;
# controller/v0.2.0-rc.2 is the last tag ahead of all three.
#
# chart-upgrade-identity-assertions.py check-baseline fails the lane if the ref
# is bumped to a chart that has already renamed, so a bad bump is loud rather
# than a lane that passes without testing anything.
CHART_UPGRADE_FROM_REF ?= controller/v0.2.0-rc.2

KIND_VERSION    ?= v0.32.0
KIND_SHA256_AMD64 ?= 50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54
KIND_SHA256_ARM64 ?= b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d
KUBECTL_VERSION ?= v1.36.0
KUBECTL_SHA256_AMD64 ?= 123d8c8844f46b1244c547fffb3c17180c0c26dac9890589fe7e67763298748e
KUBECTL_SHA256_ARM64 ?= 9f9d9c44a7b5264515ac9da5991584e2395bd50662e651132337e7b4d0c56f8f
# clusterctl and the CAPI core/kubeadm providers ship as one release, so one
# pin covers the binary and everything `clusterctl init` installs.
# Consumers: controller/Makefile.dev, controller/hack/ci/backend-e2e.sh,
# cli/Makefile, and the gofail and nvkectl bootstrap E2E workflows.
CAPI_VERSION    ?= v1.13.6
# clusterctl-linux-{amd64,arm64} from the CAPI release above.
CLUSTERCTL_SHA256_AMD64 ?= 1e4019b8ef30fa13993716d28b9fec4072fee1e17ea2cc69cb7c8f232d9b8c2f
CLUSTERCTL_SHA256_ARM64 ?= c3fe61fd44fa63d2efcee2a93432619527909b210a84cda7ee86eea1425de8c2
# The cert-manager `clusterctl init` installs at CAPI_VERSION. The NKTP chart's
# Kind e2e installs it, and its kubeconform check validates against its CRDs.
# Move it with CAPI_VERSION.
CERT_MANAGER_VERSION ?= v1.21.1
# Rancher's local-path-provisioner, the lab StorageClass of the local site
# (deploy/nvke-deploy-local.sh); a real site brings its own CSI driver.
LOCAL_PATH_PROVISIONER_VERSION ?= v0.0.31
# kind drives it as its container provider in the nvkectl bootstrap e2e, which
# installs the nerdctl-full release for this version. release/ reads this pin
# for the qualification plan.
NERDCTL_VERSION ?= 2.3.5
# Both from that release's SHA256SUMS, for the nerdctl-full archive. They move
# with NERDCTL_VERSION.
NERDCTL_FULL_SHA256_AMD64 ?= b697295c623639734aaab737523c808fd3cc8d3046039fd94fff1744e4c317aa
NERDCTL_FULL_SHA256_ARM64 ?= 6e4b687f1d138e750a3c8372abc0f81d3d7490b6359c48c0562fc7dfe98859b2

ORAS_VERSION        ?= 1.3.3
ORAS_SHA256_AMD64   ?= 9ce999f8d2de03fc03968b29d743077a58783e545e5eaa53917ca177352d0e59
PACKER_VERSION      ?= 1.15.4
# packer_<version>_linux_{amd64,arm64}.zip, from HashiCorp's SHA256SUMS.
PACKER_SHA256_AMD64 ?= 15f97a6a99645c7d5308c609973b5280837b38e112beac413ccbce80da927cf1
PACKER_SHA256_ARM64 ?= 23e6d2e596dd9e2527e0f7bea9aedd26059729375a0d17c462c2621f1b97b82d
# The S3 client the OS-image bake reads its build cache with. The ARC runner
# images carry no AWS CLI. s5cmd_<version>_Linux-{64bit,arm64}.tar.gz, from the
# release's checksums.txt.
S5CMD_VERSION      ?= 2.3.0
S5CMD_SHA256_AMD64 ?= de0fdbfa3aceae55e069ba81a0fc17b2026567637603734a387b2fca06c299b4
S5CMD_SHA256_ARM64 ?= 1439f0d00ecedcd2a2f1f2c6749bbb0152b2257bf5086f29646ec8ae38798e24
GO_LICENSES_VERSION ?= v2.0.2-0.20260709180300-7e73597a45d1
# Lints controller/api/openapi.yaml (`make -C controller/api lint-spec`).
VACUUM_VERSION      ?= v0.30.6
# Diffs controller/api/openapi.yaml against the newest published controller/api
# tag and classifies each change (`make -C controller api-compat-check`). From
# the same author as vacuum, over the same libopenapi parser, so the gate and
# the spec linter agree on what the document says.
OPENAPI_CHANGES_VERSION ?= v0.2.11
# Renders controller/api/openapi.yaml into one HTML page (`make -C controller/api docs`).
REDOCLY_VERSION     ?= 2.57.0
# Checks and publishes the documentation site (.github/workflows/api-docs.yml).
# fern/fern.config.json pins the same number; move both together.
FERN_VERSION        ?= 5.143.6

# syft generates the SPDX SBOMs that ship with every released artifact. The
# version is pinned here rather than left to anchore/sbom-action's bundled
# default, so a re-run of an old tag produces the same document a consumer
# already has, and a syft bump is a reviewable commit rather than a silent
# change in what the release describes.
SYFT_VERSION ?= v1.41.2

# Development tools invoked by module Makefiles. These were declared where they
# were used -- buf in agent/, setup-envtest and ginkgo in controller/, and
# controller-gen separately in two modules -- so nothing stopped two copies of
# the same pin drifting apart, and hack/tool-version-check.sh had nothing to
# check against. One declaration each, here.
ADDLICENSE_VERSION       ?= v1.1.1
BUF_VERSION              ?= v1.65.0
CONTROLLER_TOOLS_VERSION ?= v0.20.1
GINKGO_VERSION           ?= v2.27.2
GOFAIL_VERSION           ?= v0.2.0
GOFUMPT_VERSION          ?= v0.11.0
GOIMPORTS_VERSION        ?= v0.48.0
SETUP_ENVTEST_VERSION    ?= release-0.24
# Kubernetes version for the envtest kube-apiserver and etcd binaries.
ENVTEST_K8S_VERSION      ?= 1.34.1

# The upstream SPIRE artifacts os-image/packages/spire-agent wraps into a .deb.
# Not a development tool, but the same one-owner rule applies for the same
# reason: these two also select the server-side attestor and the bundles the
# CAPD and NICo flows build against, and every consumer has to agree with the
# version the nodes actually run.
#
# Reading this file (enforced -- a literal re-declaration fails the check):
#   os-image/packages/spire-agent/Makefile        the node .deb
#   controller/hack/ci/prepare-capd-bundles.sh    CAPD e2e bundle
#   controller/hack/capd-kindest-node/build-image.sh  CAPD/NICo node image
#
# NOT reachable by the check, because these embed the version inside an image
# tag or a package spec rather than assigning it. Bump them by hand, here, in
# the same commit:
#   controller/Tiltfile                      spire-tpm-plugin image tag + the
#                                            sha256 of the binary inside it
#   hack/mirror-bootstrap-images.sh          spire-tpm-plugin image tag
#   controller/hack/create-forge-nico-cluster.sh  apt spire-agent=<v>+tpm<v>
#
# `git grep -n '1\.14\.1\|1\.9\.0' -- controller hack os-image` finds the whole
# set; the lock under os-image/packages/spire-agent/lock/ holds the digests and
# is regenerated with `make lock`, never hand-edited.
SPIRE_VERSION      ?= 1.14.1
TPM_PLUGIN_VERSION ?= 1.9.0
