# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Teleport binaries built from source by build.sh. AGPL-3.0: the source is
# github.com/gravitational/teleport at the tag in the image name.
FROM docker.io/debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY bin/teleport bin/tctl bin/tsh /usr/local/bin/
LABEL org.opencontainers.image.source="https://github.com/gravitational/teleport" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
ENTRYPOINT ["/usr/local/bin/teleport"]
CMD ["start", "-c", "/etc/teleport/teleport.yaml"]
