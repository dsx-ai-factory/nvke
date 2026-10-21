# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.ONESHELL:
.DEFAULT_GOAL := help
REPO := $(abspath ../../..)
STATE := $(CURDIR)/.state
PYTHON ?= $(STATE)/venv/bin/python
VENV_READY := $(STATE)/venv/.requirements-installed
DOCKER_CONTEXT ?= colima
DOCKER_BUILD_FLAGS ?=
export DOCKER_CONTEXT
unexport DOCKER_HOST
include $(REPO)/make/tool-versions.mk
input = $(shell $(PYTHON) -c 'import json; print(json.load(open("$(CURDIR)/inputs.json"))["$(1)"]["$(2)"])')
OVN_REV = $(call input,ovn,revision)
OVN_VERSION = $(call input,ovn,version)
OVN_IMAGE = $(call input,ovn,image)
PLUGIN_IMAGE = $(call input,device_plugin,image)
FRR_CONTROLLER = $(call input,frr_k8s,image)
FRR_IMAGE = $(call input,frr,image)

.PHONY: help setup inputs helpers agent packages node runtime build chart-check test create resume verify scale restart cleanup
help:
	@echo 'make build: build nvke-nais-dpu-sim from pinned upstream inputs'
	@echo 'make test chart-check: run offline tests and render the three OVN roles'
	@echo 'make create CAPNICO_CHART=/path/to/chart: provision both clusters through NVKE'
	@echo 'make resume|verify|scale|restart|cleanup STATE_DIR=/path/to/run [WORKERS=2]'

setup: $(VENV_READY)
	chmod 700 "$(STATE)"
$(VENV_READY): requirements.txt
	python3 -m venv "$(STATE)/venv"
	"$(STATE)/venv/bin/pip" install -r requirements.txt
	touch "$@"

inputs: setup
	mkdir -p "$(STATE)/build/bundle/charts"
	curl -fLsS --retry 3 "https://codeload.github.com/ovn-kubernetes/ovn-kubernetes/tar.gz/$(OVN_REV)" -o "$(STATE)/ovn-source.tar.gz"
	echo '$(call input,ovn,source_sha256)  $(STATE)/ovn-source.tar.gz' | shasum -a 256 -c -
	rm -rf "$(STATE)/build/bundle/charts/ovn-kubernetes"
	tar -xzf "$(STATE)/ovn-source.tar.gz" --strip-components=2 -C "$(STATE)/build/bundle/charts" "ovn-kubernetes-$(OVN_REV)/helm/ovn-kubernetes"
	for item in device_plugin frr_k8s; do
	  case "$$item" in device_plugin) repo=ovn-kubernetes/dpu-simulator; dest=device-plugin/source ;; frr_k8s) repo=metallb/frr-k8s; dest=frr-k8s ;; esac
	  rev=$$($(PYTHON) -c "import json;print(json.load(open('inputs.json'))['$$item']['revision'])")
	  sha=$$($(PYTHON) -c "import json;print(json.load(open('inputs.json'))['$$item']['source_sha256'])")
	  curl -fLsS --retry 3 "https://codeload.github.com/$$repo/tar.gz/$$rev" -o "$(STATE)/$$item.tar.gz"
	  echo "$$sha  $(STATE)/$$item.tar.gz" | shasum -a 256 -c -
	  mkdir -p "$(STATE)/$$dest"
	  tar -xzf "$(STATE)/$$item.tar.gz" --strip-components=1 -C "$(STATE)/$$dest"
	done
	curl -fLsS --retry 3 'https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/$(call input,multus,version)/deployments/multus-daemonset-thick.yml' -o "$(STATE)/multus-original.yaml"
	echo '$(call input,multus,manifest_sha256)  $(STATE)/multus-original.yaml' | shasum -a 256 -c -
	$(PYTHON) nais-dpu-sim.py stage
	for item in ipamclaims multi_network_policy; do
	  read -r url sha < <($(PYTHON) -c "import json;v=json.load(open('inputs.json'))['$$item'];print(v['url'],v['sha256'])")
	  curl -fLsS --retry 3 "$$url" -o "$(STATE)/$$item.yaml"
	  echo "$$sha  $(STATE)/$$item.yaml" | shasum -a 256 -c -
	done
	docker pull '$(OVN_IMAGE)@$(call input,ovn,digest)'
	docker tag '$(OVN_IMAGE)@$(call input,ovn,digest)' '$(OVN_IMAGE)'
	docker pull '$(patsubst %:10.6.0,%,$(FRR_IMAGE))@$(call input,frr,digest)'
	docker tag '$(patsubst %:10.6.0,%,$(FRR_IMAGE))@$(call input,frr,digest)' '$(FRR_IMAGE)'
	skopeo copy --preserve-digests --override-os linux --override-arch arm64 'docker://$(call input,multus,image)@$(call input,multus,digest)' 'oci-archive:$(STATE)/multus-oci.tar:multus'

helpers:
	mkdir -p "$(STATE)/build/helpers"
	cd "$(STATE)/device-plugin/source"
	GOWORK=off GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$(STATE)/build/helpers/dpu-sim-dp" ./cmd/dpu-sim-dp
	cd "$(STATE)/frr-k8s"
	for item in frr-k8s:cmd/frr-k8s-controller/main.go frr-metrics:cmd/metrics/exporter.go frr-status:cmd/status/exporter.go statuscleaner:cmd/statuscleaner/main.go; do
	  GOWORK=off GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$(STATE)/build/helpers/$${item%%:*}" "$${item#*:}"
	done
	cp cmd/reloader/frr-reloader.sh LICENSE "$(STATE)/build/helpers/"
	cd "$(CURDIR)"
	docker build $(DOCKER_BUILD_FLAGS) --target device-plugin -f images/Dockerfile.helpers -t '$(PLUGIN_IMAGE)' "$(STATE)/build/helpers"
	docker build $(DOCKER_BUILD_FLAGS) --target frr -f images/Dockerfile.helpers -t '$(FRR_CONTROLLER)' "$(STATE)/build/helpers"

agent:
	mkdir -p "$(STATE)/build/node"
	$(MAKE) -C "$(REPO)/agent" deb-arm64 AGENT_SKIP_LINKAGE_CHECK=1 DEB_OUTPUT="$(STATE)/build/node/agent.deb"

packages: agent
	PACKAGE_NAME=nvke-cni-ovn-charts-$(OVN_VERSION) STREAM=$(OVN_VERSION) DEB_REVISION=1 ARCH=arm64 \
	  SRC="$(STATE)/build/bundle" IMAGES_PACKAGE=nvke-cni-ovn-images-$(OVN_VERSION) \
	  "$(REPO)/agent/bin/nfpm-v$(NFPM_VERSION)/nfpm" package \
	  -f "$(REPO)/os-image/packages/cni-ovn/packaging/nfpm-charts.yaml" -p deb -t "$(STATE)/build/node/charts.deb"

node:
	docker save -o "$(STATE)/build/node/images.tar" '$(OVN_IMAGE)' '$(PLUGIN_IMAGE)' '$(FRR_CONTROLLER)' '$(FRR_IMAGE)'
	cp images/preload-images.sh "$(STATE)/multus-oci.tar" "$(STATE)/build/node/"
	printf '%s\n' '$(call input,multus,image)@$(call input,multus,digest)' > "$(STATE)/build/node/multus-image"
	docker build $(DOCKER_BUILD_FLAGS) -f images/Dockerfile.managed-node -t nvke-nais-dpu-sim-node:local "$(STATE)/build/node"

runtime:
	mkdir -p "$(STATE)/build/runtime"
	$(MAKE) -C "$(REPO)/controller" build GOOS=linux GOARCH=arm64
	cp "$(REPO)/controller/bin/controller" "$(STATE)/build/runtime/"
	docker run --rm --name nvke-nais-dpu-sim-build-runtime -v "$(REPO):/src" -w /src/controller/nico-sim golang:1.27.2-alpine sh -ec 'apk add --no-cache gcc musl-dev make; make build-static BINARY=/src/controller/hack/nais-dpu-sim/.state/build/runtime/nico-sim'
	cp "$(REPO)/LICENSE" "$(REPO)/NOTICE" "$(REPO)/controller/THIRD_PARTY_NOTICES.md" "$(STATE)/build/runtime/"
	docker build $(DOCKER_BUILD_FLAGS) -f images/Dockerfile.managed-runtime -t nvke-nais-dpu-sim-runtime:local "$(STATE)/build/runtime"
	docker image inspect cluster-api-provider-nico:local >/dev/null
	docker pull temporalio/temporal:1.9.1
	docker pull postgres:17.4

build:
	$(MAKE) inputs
	$(MAKE) helpers
	$(MAKE) packages
	$(MAKE) node
	$(MAKE) runtime
	$(MAKE) chart-check

test: setup
	$(PYTHON) -B -m unittest discover -s tests -v

chart-check:
	$(PYTHON) nais-dpu-sim.py chart-check
create:
	@test -n "$(CAPNICO_CHART)" || { echo 'Set CAPNICO_CHART to the CAPNICo Helm chart.' >&2; exit 1; }
	$(PYTHON) nais-dpu-sim.py create --provider-chart "$(CAPNICO_CHART)" --docker-context "$(DOCKER_CONTEXT)"
resume verify scale restart cleanup:
	@test -n "$(STATE_DIR)" || { echo 'Set STATE_DIR to the run directory.' >&2; exit 1; }
	$(PYTHON) nais-dpu-sim.py $@ --state "$(STATE_DIR)" $(if $(filter scale,$@),--workers $(WORKERS),) $(if $(and $(filter resume,$@),$(CAPNICO_CHART)),--provider-chart "$(CAPNICO_CHART)",)
