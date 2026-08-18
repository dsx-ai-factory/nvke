// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	defaultHealthCheckTimeout = 2 * time.Second
	defaultHealthCacheTTL     = 5 * time.Second
)

type dependencyCheck struct {
	name  string
	check func(ctx context.Context) error
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

type healthChecker struct {
	logger       *slog.Logger
	checks       []dependencyCheck
	checkTimeout time.Duration
	cacheTTL     time.Duration

	mu       sync.Mutex
	cached   healthResponse
	cachedAt time.Time
}

func newHealthChecker(logger *slog.Logger, database db.DB, temporalClient client.Client) *healthChecker {
	return &healthChecker{
		logger: logger,
		checks: []dependencyCheck{
			{
				name: databaseConst,
				check: func(ctx context.Context) error {
					if database == nil {
						return errors.New("database is not configured")
					}
					return database.Ping(ctx)
				},
			},
			{
				name: temporal,
				check: func(ctx context.Context) error {
					if temporalClient == nil {
						return errors.New("temporal client is not configured")
					}
					_, err := temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{})
					return err
				},
			},
		},
		checkTimeout: defaultHealthCheckTimeout,
		cacheTTL:     defaultHealthCacheTTL,
	}
}

func (h *healthChecker) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(healthResponse{Status: okValue}); err != nil {
		h.logger.Error("Failed to write /healthz response", "error", err)
	}
}

func (h *healthChecker) readyz(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cachedAt.IsZero() || time.Since(h.cachedAt) >= h.cacheTTL {
		h.cached = h.runChecks(context.WithoutCancel(r.Context()))
		h.cachedAt = time.Now()
	}

	statusCode := http.StatusOK
	if h.cached.Status != okValue {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(h.cached); err != nil {
		h.logger.Error("Failed to write /readyz response", "error", err)
	}
}

func (h *healthChecker) runChecks(ctx context.Context) healthResponse {
	type checkResult struct {
		name string
		err  error
	}

	results := make(chan checkResult, len(h.checks))
	for _, dependency := range h.checks {
		go func() {
			checkCtx, cancel := context.WithTimeout(ctx, h.checkTimeout)
			defer cancel()
			results <- checkResult{name: dependency.name, err: dependency.check(checkCtx)}
		}()
	}

	response := healthResponse{
		Status: okValue,
		Checks: make(map[string]string, len(h.checks)),
	}
	for range h.checks {
		result := <-results
		if result.err == nil {
			response.Checks[result.name] = okValue
			continue
		}

		response.Status = "unavailable"
		response.Checks[result.name] = result.err.Error()
		h.logger.Warn("Health dependency check failed", "dependency", result.name, "error", result.err)
	}

	return response
}
