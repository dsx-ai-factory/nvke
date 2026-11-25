// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func dbNodeAgentSettingsFromAPI(settings *api.NodeAgentSettings) *db.NodeAgentSettings {
	if settings == nil {
		return nil
	}
	out := &db.NodeAgentSettings{}
	if settings.TokenFrom != nil {
		out.TokenSource = db.NodeAgentTokenSource(*settings.TokenFrom)
	}
	return out
}

func apiNodeAgentSettingsFromDB(settings *db.NodeAgentSettings) *api.NodeAgentSettings {
	if settings == nil {
		return nil
	}
	out := &api.NodeAgentSettings{}
	if settings.TokenSource != "" {
		tokenFrom := api.NodeAgentTokenSource(settings.TokenSource)
		out.TokenFrom = &tokenFrom
	}
	return out
}

func dbNodeAgentTokenSourceFromAPI(tokenFrom *api.NodeAgentTokenSource) db.NodeAgentTokenSource {
	if tokenFrom == nil {
		return ""
	}
	return db.NodeAgentTokenSource(*tokenFrom)
}

func apiNodeAgentTokenSourceFromDB(tokenSource db.NodeAgentTokenSource) *api.NodeAgentTokenSource {
	if tokenSource == "" {
		return nil
	}
	out := api.NodeAgentTokenSource(tokenSource)
	return &out
}
