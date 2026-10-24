# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The E2E suite as an image, for validating a running site from inside its
# cluster, under an in-cluster test runner. It carries the whole suite and runs
# the specs NVKE_SITE_VALIDATE and NVKE_TENANT_VALIDATE select, the site's by
# default: specs that install and change nothing lasting on the site or the
# tenant. Those labeled creates, and the DNS probe, make short-lived objects in
# the probe namespace and delete them.
#
# Build from the REPOSITORY ROOT, so the context includes controller/api,
# which go.mod replaces with a relative path:
#   docker buildx build -f controller/test/e2e/Dockerfile .
# Dockerfile.dockerignore next to this file governs the context, not the root
# .dockerignore: it admits only the two modules, so a local e2e-config.yaml or
# run state under this directory never reaches a layer or the build cache.
#
# Bases are the controller image's, digest-pinned and refreshed by
# hack/base-image-update.sh.
#
# The build is reproducible: the same commit, build args and flags give the same
# image digest wherever it is built, so a site that builds this itself can
# compare its digest with the one the release attested. `make
# validate-image-push` passes the args and flags; "Building the image yourself"
# in README.md lists them.
ARG GOLANG_IMAGE=docker.io/golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc
ARG ALPINE_IMAGE=docker.io/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# On the build platform, cross-compiling, so a multi-arch build needs no
# emulation.
FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS builder
# A module mirror, for a builder that cannot reach proxy.golang.org. It changes
# where modules come from, not which: go.sum still decides.
ARG GOPROXY=https://proxy.golang.org|direct
ARG SOURCE_DATE_EPOCH
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY . /workspace
WORKDIR /workspace/controller/test/e2e
# GOTOOLCHAIN=local, so the compiler is the base image's and never one go.mod
# asks to download. The context carries no .git, so there is no VCS stamp to
# vary either.
RUN GOWORK=off GOTOOLCHAIN=local GOPROXY="${GOPROXY}" CGO_ENABLED=0 \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go test -c -trimpath -buildvcs=false -o /out/e2e.test .
# rewrite-timestamp only clamps times later than SOURCE_DATE_EPOCH, so a file
# copied from the context keeps an older mtime the checkout happened to give it.
# Stamped here instead, so the final stage copies only files this build wrote.
RUN mkdir /out/licenses && cp /workspace/LICENSE /workspace/NOTICE /out/licenses/ && \
    touch -d "@${SOURCE_DATE_EPOCH:-0}" /out/licenses /out/licenses/*

# No RUN: a cross-built target needs no emulation, and alpine (rather than
# distroless) so a runner can still wrap the binary in a shell script.
FROM ${ALPINE_IMAGE}
COPY --from=builder /out/e2e.test /usr/local/bin/e2e.test
COPY --from=builder /out/licenses/ /licenses/

# Reports go to /artifacts, which the runner mounts: junit-test-report.xml at
# its root, the name runners that collect JUnit look for, and report.json.
WORKDIR /e2e
USER 65532:65532

# A run that names no expected version fails rather than skipping the check,
# which a report would show as a pass. The two switches choose the specs, the
# site's alone unless a run turns on its tenant's; an explicit
# -ginkgo.label-filter overrides them.
ENV NVKE_E2E_REQUIRE_EXPECTED_VERSION=true \
    NVKE_SITE_VALIDATE=true \
    NVKE_TENANT_VALIDATE=false

# Build args rather than --label, so a rebuild that passes the same values
# writes the same config, and so the same digest.
ARG VALIDATE_VERSION=""
ARG VALIDATE_REVISION=""
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="nvke-validate" \
      org.opencontainers.image.source="https://github.com/dsx-ai-factory/nvke" \
      org.opencontainers.image.description="NVKE site validation suite" \
      org.opencontainers.image.version="${VALIDATE_VERSION}" \
      org.opencontainers.image.revision="${VALIDATE_REVISION}"

# The report paths are in ENTRYPOINT so that arguments a caller adds, such as
# -e2e.versions, extend the run rather than replace it. Spec selection is not
# the guard: the identity the image runs as is what keeps a mutating spec from
# running.
#
# -test.run because the binary also carries the module's unit tests, some of
# which need a git checkout. The JUnit report is the suite's, which lists only
# the selected specs; report.json is Ginkgo's, with every spec. A spec still
# running after two minutes reports its current step and stack every minute,
# so a hung wait shows where it is rather than only its final error.
#
# -ginkgo.timeout because Ginkgo's default is an hour: a healthy site finishes
# well inside it, but on a broken one every check waits out its own limit,
# which together run past two hours, and the checks still to run when the
# suite times out are reported as interrupted, not as what they found. A
# caller's own -ginkgo.timeout, given later, wins.
ENTRYPOINT ["/usr/local/bin/e2e.test", \
    "-test.run=^TestE2E$", \
    "-ginkgo.timeout=3h", \
    "-e2e.junit-report=/artifacts/junit-test-report.xml", \
    "-ginkgo.json-report=/artifacts/report.json", \
    "-ginkgo.silence-skips", \
    "-ginkgo.no-color", \
    "-ginkgo.poll-progress-after=2m", \
    "-ginkgo.poll-progress-interval=1m", \
    "-ginkgo.v"]
