# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PACKER      ?= packer
SSH_DIR     := $(CURDIR)/.ssh
ARTIFACTS   := $(CURDIR)/artifacts
PKR_FILE    := $(CURDIR)/nke-node.pkr.hcl

NKE_AGENT_VERSION      ?= edge
NKE_AGENT_REGISTRY     ?= ghcr.io/nvidia/nke/agent
NKE_AGENT_DEB_REGISTRY ?= ghcr.io/nvidia/nke/agent-deb

.PHONY: os-image image init ssh-key clean check-deps

ssh-key: $(SSH_DIR)/id_ed25519

$(SSH_DIR)/id_ed25519:
	mkdir -p $(SSH_DIR)
	ssh-keygen -t ed25519 -N '' -f $(SSH_DIR)/id_ed25519

init:
	$(PACKER) init $(PKR_FILE)

check-deps:
	@command -v oras >/dev/null 2>&1 || { \
		echo "oras is required to pull $(NKE_AGENT_DEB_REGISTRY) (or $(NKE_AGENT_REGISTRY))" >&2; \
		echo "https://oras.land/docs/installation" >&2; \
		exit 1; \
	}

os-image: check-deps ssh-key init
	mkdir -p $(ARTIFACTS)
	NKE_AGENT_VERSION=$(NKE_AGENT_VERSION) \
	NKE_AGENT_REGISTRY=$(NKE_AGENT_REGISTRY) \
	NKE_AGENT_DEB_REGISTRY=$(NKE_AGENT_DEB_REGISTRY) \
	$(PACKER) build \
		-var nke_agent_version=$(NKE_AGENT_VERSION) \
		-var nke_agent_registry=$(NKE_AGENT_REGISTRY) \
		-var nke_agent_deb_registry=$(NKE_AGENT_DEB_REGISTRY) \
		$(PKR_FILE)

# Alias kept so existing `make image` muscle memory still works.
image: os-image

clean:
	rm -rf $(CURDIR)/output $(ARTIFACTS)
