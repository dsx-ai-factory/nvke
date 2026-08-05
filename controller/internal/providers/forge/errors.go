// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package forge

import (
	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

var (
	// Error aliases from the api layer to avoid explicit error mapping.

	ErrInstanceAlreadyExists = api.ErrInstanceAlreadyExists
	ErrInstanceNotFound      = api.ErrInstanceNotFound
	ErrBadRequest            = api.ErrBadRequest
	ErrSubnetExhausted       = api.ErrSubnetExhausted
	ErrVpcPrefixExhausted    = api.ErrVpcPrefixExhausted
)
