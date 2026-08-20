# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PACKER      ?= packer
SSH_DIR     := $(CURDIR)/.ssh
ARTIFACTS   := $(CURDIR)/artifacts
PKR_FILE    := $(CURDIR)/nke-node.pkr.hcl

NKE_AGENT_VERSION  ?= edge
NKE_AGENT_REGISTRY ?= ghcr.io/nvidia/nke/agent

.PHONY: image init ssh-key clean

ssh-key: $(SSH_DIR)/id_ed25519

$(SSH_DIR)/id_ed25519:
	mkdir -p $(SSH_DIR)
	ssh-keygen -t ed25519 -N '' -f $(SSH_DIR)/id_ed25519

init:
	$(PACKER) init $(PKR_FILE)

image: ssh-key init
	NKE_AGENT_VERSION=$(NKE_AGENT_VERSION) \
	NKE_AGENT_REGISTRY=$(NKE_AGENT_REGISTRY) \
	$(PACKER) build \
		-var nke_agent_version=$(NKE_AGENT_VERSION) \
		-var nke_agent_registry=$(NKE_AGENT_REGISTRY) \
		$(PKR_FILE)

clean:
	rm -rf $(CURDIR)/output $(ARTIFACTS)
