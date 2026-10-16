# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The stream this directory builds. Bump here and run `make lock`; see
# README.md. bundle.sh takes X.Y.0 only, because it fetches the upstream tag
# v$(OVN_STREAMS), so this can name a version with an upstream release and
# nothing else.
OVN_STREAMS ?= 1.4.0

# The stream a qualification run installs, and the agent's default when an
# InstallCNI request names no version (defaultOvnCniChartStream in
# agent/internal/agent/cni.go). release/registry.yaml reads it.
#
# Separate from OVN_STREAMS because the two have diverged: the published
# nvke-cni-ovn-* packages are built by cni-packages, where the version is a
# chart stream plus a patch counter rather than an upstream tag, and 1.4.0 --
# the newest this directory can build -- predates the rename, so it exists
# only under the old name and was never built for 26.04.
OVN_QUALIFY_STREAM ?= 1.4.11

CNI := cni-ovn
PRODUCT := OVN
VERSIONS := $(OVN_STREAMS)
CONTENT_SPEC := $(CURDIR)/packaging/nfpm-charts.yaml
CONTENT_KIND := charts

include ../cni-common/deb.mk
