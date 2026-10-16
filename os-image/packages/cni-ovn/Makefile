# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The dsx-ai-factory/ovn-kubernetes chart stream the agent installs by default
# (defaultOvnCniChartStream in agent/internal/agent/cni.go). Bump here and run
# `make lock`; see README.md.
OVN_STREAMS ?= 2.4.2

CNI := cni-ovn
PRODUCT := OVN
VERSIONS := $(OVN_STREAMS)
CONTENT_SPEC := $(CURDIR)/packaging/nfpm-charts.yaml
CONTENT_KIND := charts

include ../cni-common/deb.mk
