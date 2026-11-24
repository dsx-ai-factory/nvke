// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func dbNodeIdentityFromAPI(identity *api.NodeIdentity) db.NodeIdentity {
	if identity == nil {
		return ""
	}
	return db.NodeIdentity(*identity)
}

func apiNodeIdentityFromDB(identity db.NodeIdentity) *api.NodeIdentity {
	if identity == "" {
		return nil
	}
	out := api.NodeIdentity(identity)
	return &out
}
