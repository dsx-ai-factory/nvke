// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
