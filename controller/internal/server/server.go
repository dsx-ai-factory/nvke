// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	pathpkg "path"
	"time"

	oapi_middleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/swaggest/swgui/v5emb"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/version"
)

const (
	ReadHeaderTimeout = 5 * time.Second
)

func NewServer(
	cfg config.Config,
	logger *slog.Logger,
	database db.DB,
	forgeProvider *forge.ForgeProvider,
	temporalClient client.Client,
) (*http.Server, error) {
	m := http.NewServeMux()

	openapiSpec, err := api.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}
	serverImpl := NewServerImpl(cfg, logger, database, forgeProvider, temporalClient)
	strictHandler := api.NewStrictHandlerWithOptions(serverImpl, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("OpenAPI StrictServer request error", "error", err)
			handleError(w, logger, err.Error(), http.StatusBadRequest)
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("OpenAPI StrictServer response error", "error", err)
			handleError(w, logger, err.Error(), http.StatusInternalServerError)
		},
	})

	api.HandlerWithOptions(strictHandler, api.StdHTTPServerOptions{
		BaseRouter: m,
		BaseURL:    "/api",
		Middlewares: []api.MiddlewareFunc{
			oapi_middleware.OapiRequestValidatorWithOptions(openapiSpec, &oapi_middleware.Options{
				SilenceServersWarning: true,
				ErrorHandler: func(w http.ResponseWriter, message string, statusCode int) {
					logger.Error("OpenAPI middleware validation error", "message", message)
					handleError(w, logger, message, statusCode)
				},
			}),
		},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("OpenAPI StdServer error", "error", err)
			handleError(w, logger, err.Error(), stdServerErrorStatus(err))
		},
	})

	m.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		if _, err := w.Write([]byte(api.OpenapiYAML)); err != nil {
			logger.Error("Failed to write /openapi.yaml response", "error", err)
		}
	})

	m.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"version":           version.Version,
			"temporalUIURL":     temporalUIURL(cfg),
			"temporalNamespace": cfg.TemporalNamespace,
			// Exposes the control-plane-controller provisioning feature flag so the
			// UI can switch between cluster-level control plane config mode and the
			// legacy control-plane node-pool mode. The server validates regardless.
			"provisionControlPlaneWithControlPlaneController": cfg.UseControlPlaneControllerForControlPlane(),
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.Error("Failed to write /api/version response", "error", err)
		}
	})

	m.Handle("/api/docs/", v5emb.New("CWE API", "/openapi.yaml", "/api/docs/"))

	// Serve a simple page at /docs with a message that the swagger UI is now at /api/docs
	m.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`The Swagger UI is now at /api/docs/`)); err != nil {
			logger.Error("Failed to write /docs response", "error", err)
		}
	})

	hc := newHealthChecker(logger, database, temporalClient)
	m.HandleFunc("GET "+healthz, hc.healthz)
	m.HandleFunc("GET "+readyz, hc.readyz)

	m.Handle("/", http.FileServer(http.Dir(cfg.UIDir)))

	// Apply middleware chain: logging -> managed-by check -> main handler
	handler := loggingMiddleware(m, logger)
	handler = managedByMiddleware(cfg.ManagedByK8sPlatform, logger)(handler)
	if cfg.Tracing.Enabled {
		handler = otelhttp.NewHandler(handler, "cloud-workflow-engine",
			otelhttp.WithFilter(func(r *http.Request) bool {
				// Skip operational probes (/healthz, /readyz, /metrics). /metrics is
				// served on a separate listen address today, but is excluded here too
				// so it stays consistent with isOperationalPath.
				return !isOperationalPath(pathpkg.Clean(r.URL.Path))
			}),
		)
	}

	srv := &http.Server{
		Addr:              cfg.ServerListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}

	return srv, nil
}

// stdServerErrorStatus maps oapi-codegen-generated request-parsing errors to
// HTTP 400 (client errors) and falls back to HTTP 500 for everything else.
func stdServerErrorStatus(err error) int {
	var (
		invalidParamFormat    *api.InvalidParamFormatError
		unmarshalingParam     *api.UnmarshalingParamError
		requiredParam         *api.RequiredParamError
		requiredHeader        *api.RequiredHeaderError
		tooManyValuesForParam *api.TooManyValuesForParamError
		unescapedCookieParam  *api.UnescapedCookieParamError
	)
	switch {
	case errors.As(err, &invalidParamFormat),
		errors.As(err, &unmarshalingParam),
		errors.As(err, &requiredParam),
		errors.As(err, &requiredHeader),
		errors.As(err, &tooManyValuesForParam),
		errors.As(err, &unescapedCookieParam):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func handleError(w http.ResponseWriter, logger *slog.Logger, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(
		api.GenericError{Message: message},
	); err != nil {
		logger.Error("Failed to encode error response", "error", err)
	}
}

func NewServerImpl(cfg config.Config, logger *slog.Logger, database db.DB, forgeProvider *forge.ForgeProvider, temporalClient client.Client) *serverImpl {
	nodePoolService := NewNodePoolService(logger, database, forgeProvider, cfg.Teleport, cfg.UseControlPlaneControllerForControlPlane())

	return &serverImpl{
		siteService:          *NewSiteService(logger, database, forgeProvider),
		clusterService:       *NewClusterService(logger, database, nodePoolService, forgeProvider, cfg.Teleport, cfg.EnableForgeCapi, cfg.NkeOIDCAuth.Enabled, cfg.UseControlPlaneControllerForControlPlane()),
		nodePoolService:      *nodePoolService,
		nodeSetService:       *NewNodeSetService(logger, database, temporalClient),
		nodeService:          *NewNodeService(logger, database, temporalClient, cfg.AllowEtcdSafetyOverrides),
		osProfileService:     *NewOSProfileService(cfg, logger),
		configProfileService: *NewConfigProfileService(cfg, logger),
		forgeService:         *NewForgeService(cfg, logger, forgeProvider),
	}
}

type serverImpl struct {
	siteService
	clusterService
	nodePoolService
	nodeSetService
	nodeService
	osProfileService
	configProfileService
	forgeService
}

// temporalUIURL returns the Temporal Web UI base URL to surface in the version
// response. An explicit TEMPORAL_UI_URL always wins (e.g. Tilt sets this to
// the local port-forward). Otherwise it is derived from the Teleport proxy
// address and site controller name, which are always set in non-local
// deployments.
func temporalUIURL(cfg config.Config) string {
	if cfg.TemporalUIURL != "" {
		return cfg.TemporalUIURL
	}
	if cfg.Teleport.ProxyAddress == "" || cfg.NKESiteControllerName == "" {
		return ""
	}
	host := cfg.Teleport.ProxyAddress
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return fmt.Sprintf("https://temporal-web-%s.%s", cfg.NKESiteControllerName, host)
}
