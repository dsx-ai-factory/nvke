// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

@id("catalog-reader")
permit(
    principal in NVKE::Role::"catalog-reader",
    action in [
        NVKE::Action::"getClusterRelease",
        NVKE::Action::"listClusterReleases",
        NVKE::Action::"getMachineImage",
        NVKE::Action::"listMachineImages",
        NVKE::Action::"getControlPlaneClass",
        NVKE::Action::"listControlPlaneClasses",
        NVKE::Action::"getClusterReleaseChannel",
        NVKE::Action::"listClusterReleaseChannels",
        NVKE::Action::"getNodeProfile",
        NVKE::Action::"listNodeProfiles"
    ],
    resource
);

@id("catalog-admin")
permit(
    principal in NVKE::Role::"catalog-admin",
    action in [
        NVKE::Action::"createClusterRelease",
        NVKE::Action::"updateClusterRelease",
        NVKE::Action::"deleteClusterRelease",
        NVKE::Action::"createMachineImage",
        NVKE::Action::"updateMachineImage",
        NVKE::Action::"deleteMachineImage",
        NVKE::Action::"createControlPlaneClass",
        NVKE::Action::"updateControlPlaneClass",
        NVKE::Action::"deleteControlPlaneClass",
        NVKE::Action::"createClusterReleaseChannel",
        NVKE::Action::"updateClusterReleaseChannel",
        NVKE::Action::"deleteClusterReleaseChannel",
        NVKE::Action::"createNodeProfile",
        NVKE::Action::"updateNodeProfile",
        NVKE::Action::"deleteNodeProfile"
    ],
    resource
);
