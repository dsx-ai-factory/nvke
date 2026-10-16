# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The stream this directory builds. Bump here and run `make lock`; see
# README.md. bundle.sh takes X.Y.0 only, because it fetches the upstream tag
# v$(OVN_STREAMS), so this can name a version with an upstream release and
# nothing else.
OVN_STREAMS ?= 1.4.0

CNI := cni-ovn
PRODUCT := OVN
VERSIONS := $(OVN_STREAMS)
CONTENT_SPEC := $(CURDIR)/packaging/nfpm-charts.yaml
CONTENT_KIND := charts

include ../cni-common/deb.mk
