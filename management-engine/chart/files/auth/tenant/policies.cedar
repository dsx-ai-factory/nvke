// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

@id("cluster-viewer")
permit(
    principal in NVKE::Role::"cluster-viewer",
    action in [
        NVKE::Action::"getCluster",
        NVKE::Action::"listClusters",
        NVKE::Action::"getControlPlaneClass",
        NVKE::Action::"listControlPlaneClasses",
        NVKE::Action::"getClusterReleaseChannel",
        NVKE::Action::"listClusterReleaseChannels",
        NVKE::Action::"getNodeProfile",
        NVKE::Action::"listNodeProfiles"
    ],
    resource
);

@id("cluster-editor")
permit(
    principal in NVKE::Role::"cluster-editor",
    action in [
        NVKE::Action::"createCluster",
        NVKE::Action::"updateCluster",
        NVKE::Action::"deleteCluster"
    ],
    resource
);
