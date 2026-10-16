# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The ovn-kubernetes chart stream the agent installs by default
# (defaultOvnCniChartStream in agent/internal/agent/cni.go). Bump here and run
# `make lock`; see README.md.
OVN_STREAMS ?= 1.4.0

CNI := cni-ovn
PRODUCT := OVN
VERSIONS := $(OVN_STREAMS)
CONTENT_SPEC := $(CURDIR)/packaging/nfpm-charts.yaml
CONTENT_KIND := charts

include ../cni-common/deb.mk
