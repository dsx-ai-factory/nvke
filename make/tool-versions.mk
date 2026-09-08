# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Canonical tool versions. This file contains values only so module Makefiles
# can include it without adding root targets to their help output.

GOVULNCHECK_GO_VERSION ?= 1.26.6
GOVULNCHECK_VERSION    ?= v1.6.0
GOVULNCHECK_WAIVED_MODULES ?= controller/nico-vms

ACTIONLINT_VERSION ?= 1.7.12
SHELLCHECK_VERSION ?= 0.11.0
GITLEAKS_VERSION   ?= 8.30.1

KUBECONFORM_VERSION ?= v0.7.0
HELM_VERSION         ?= v4.2.4

# crane and nfpm build the CNI debs under os-image/packages/.
CRANE_VERSION ?= 0.21.9
NFPM_VERSION  ?= 2.47.0

KIND_VERSION    ?= v0.32.0
KUBECTL_VERSION ?= v1.36.0
# clusterctl and the CAPI core/kubeadm providers ship as one release, so one
# pin covers the binary and everything `clusterctl init` installs.
CAPI_VERSION    ?= v1.12.3

ORAS_VERSION        ?= 1.3.3
PACKER_VERSION      ?= 1.14.2
GO_LICENSES_VERSION ?= v2.0.1
