// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"log/slog"
	"sync"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge/cache"
)

// ForgeProvider is a struct that provides access to the Forge API and caching mechanisms.
// It's purpose is to marry the API interactions with caching strategies to improve performance and
// prevent the consumer from having to manage caching logic directly.
//
// If the consumer wants caching, use this Forge Provider.
// If the consumer does NOT want caching, use the Forge API package instead.
type ForgeProvider struct {
	cfg    config.Config
	db     db.DB
	logger *slog.Logger

	cacheEngine *cache.ForgeCacheEngine

	instanceMutationMu sync.Mutex
}

// NewForgeProvider creates a new instance of ForgeProvider.
func NewForgeProvider(cfg config.Config, db db.DB, logger *slog.Logger) *ForgeProvider {
	return &ForgeProvider{
		cfg:         cfg,
		db:          db,
		logger:      logger,
		cacheEngine: cache.NewForgeCacheEngine(cfg, db, logger),
	}
}

// GetCacheEngine returns the cache engine used by the ForgeProvider.
func (p *ForgeProvider) GetCacheEngine() *cache.ForgeCacheEngine {
	return p.cacheEngine
}
