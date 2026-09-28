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

KIND_VERSION    ?= v0.32.0
KIND_SHA256_AMD64 ?= 50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54
KIND_SHA256_ARM64 ?= b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d
KUBECTL_VERSION ?= v1.36.0
KUBECTL_SHA256_AMD64 ?= 123d8c8844f46b1244c547fffb3c17180c0c26dac9890589fe7e67763298748e
KUBECTL_SHA256_ARM64 ?= 9f9d9c44a7b5264515ac9da5991584e2395bd50662e651132337e7b4d0c56f8f
# clusterctl and the CAPI core/kubeadm providers ship as one release, so one
# pin covers the binary and everything `clusterctl init` installs.
CAPI_VERSION    ?= v1.12.3
# The cert-manager `clusterctl init` installs at CAPI_VERSION. The NKTP chart's
# Kind e2e installs it, and its kubeconform check validates against its CRDs.
# Move it with CAPI_VERSION.
CERT_MANAGER_VERSION ?= v1.19.3

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
# Renders controller/api/openapi.yaml into one HTML page (`make -C controller/api docs`).
REDOCLY_VERSION     ?= 2.57.0
# Checks and publishes the documentation site (.github/workflows/api-docs.yml).
# fern/fern.config.json pins the same number; move both together.
FERN_VERSION        ?= 5.143.6

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
