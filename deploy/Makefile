# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

NVKECTL := $(CURDIR)/testdata/bin/nvkectl

.PHONY: test nvkectl
test: nvkectl ## Run the executor tests in testdata/ against a freshly built nvkectl.
	NVKECTL=$(NVKECTL) ./testdata/test_nvke_deploy.sh

nvkectl: ## Build nvkectl from ../cli for the tests.
	go build -C ../cli -o $(NVKECTL) ./cmd/nvkectl
