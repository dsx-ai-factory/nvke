# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Paths resolve from this file, so `make -C deploy test` and `make -f
# deploy/Makefile test` do the same thing.
DEPLOY_DIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
REPO_DIR := $(abspath $(DEPLOY_DIR)/..)
NVKECTL := $(REPO_DIR)/cli/bin/nvkectl

.PHONY: test nvkectl
test: nvkectl ## Run the executor tests in testdata/ against a freshly built nvkectl.
	NVKECTL=$(NVKECTL) $(DEPLOY_DIR)/testdata/test_nvke_deploy.sh

nvkectl: ## Build nvkectl with the cli module's own build target.
	$(MAKE) -C $(REPO_DIR)/cli build
