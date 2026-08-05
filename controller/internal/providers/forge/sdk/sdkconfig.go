// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package sdk

import (
	"maps"

	"github.com/NVIDIA/nke/controller/internal/config"
	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
	forgesdkhelper "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go/helper"
)

// envServerIndexMap maps a ForgeEnvironment string to the corresponding SDK server index.
// The SDK's default server list has production at 0 and staging at 1; we append the
// docker-test server and record its index so ContextServerIndex can select it.
var envServerIndexMap = func() map[string]int { //nolint:gochecknoglobals // computed once at init
	m := maps.Clone(forgesdkhelper.NGCEnvServerIndexMap)
	m[config.ForgeEnvironmentTest] = len(m)
	return m
}()

// NewSDKConfig returns a Forge SDK configuration that appends cfg.ForgeURL as an
// additional server entry. The entry is selected at request time via ContextServerIndex.
func NewSDKConfig(cfg *config.Config) *forgesdk.Configuration {
	sdkCfg := forgesdk.NewConfiguration()
	sdkCfg.Servers = append(sdkCfg.Servers, forgesdk.ServerConfiguration{
		URL:         cfg.ForgeURL,
		Description: config.ForgeEnvironmentTest,
	})
	return sdkCfg
}

// ContextServerIndex returns the SDK server-list index for cfg.ForgeEnvironment.
// Pass the result as context.WithValue(ctx, forgesdk.ContextServerIndex, idx) before
// making any SDK API call.
func ContextServerIndex(cfg *config.Config) int {
	return envServerIndexMap[cfg.ForgeEnvironment]
}
