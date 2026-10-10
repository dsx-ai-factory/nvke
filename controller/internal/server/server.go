// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	pathpkg "path"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	oapi_middleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/swaggest/swgui/v5emb"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.temporal.io/sdk/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/internal/auth"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nke/controller/internal/version"
)

const (
	ReadHeaderTimeout = 5 * time.Second
)

// Option configures NewServer and NewServerImpl.
type Option func(*options)

type options struct {
	fleetOIDC *controllerconfig.OIDCConfig
}

// WithFleetOIDC sets the nke-controller-config OIDC section new clusters are pinned from.
func WithFleetOIDC(oidc *controllerconfig.OIDCConfig) Option {
	return func(o *options) { o.fleetOIDC = oidc }
}

// NewServer builds the API's HTTP server. A nil authn leaves the authentication
// middleware out of the chain rather than installing one that admits everyone.
func NewServer(
	cfg config.Config,
	logger *slog.Logger,
	database db.DB,
	temporalClient client.Client,
	authn *auth.Authenticator,
	k8sReader ctrlclient.Reader,
	opts ...Option,
) (*http.Server, error) {
	m := http.NewServeMux()

	openapiSpec, err := api.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}
	serverImpl := NewServerImpl(cfg, logger, database, temporalClient, k8sReader, opts...)
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
				// api/openapi.yaml declares `security` globally, so without an
				// AuthenticationFunc kin-openapi answers 401 to every request,
				// even with authentication off. This makes its check a no-op.
				// Tokens are verified by the middleware wrapping this mux.
				Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
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

	m.Handle("/api/docs/", v5emb.New("Controller API", "/openapi.yaml", "/api/docs/"))

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

	// Built inside out: each assignment wraps the handler the previous one
	// produced, so the order below is the reverse of the order a request meets.
	// A request runs
	//
	//	tracing -> logging -> authentication -> managed-by -> mux
	//
	// Authentication precedes managed-by so an unauthenticated caller never
	// reaches it, and follows logging so a rejection still produces the usual
	// request/response pair. Tracing stays outermost so a rejected request is
	// still a span. TestDisabledServerLogsTheManagedByRejection pins the logging
	// and managed-by order.
	handler := managedByMiddleware(cfg.ManagedByK8sPlatform, logger)(m)
	if authn != nil {
		handler = authn.Wrap(handler)
	}
	handler = otelx.ExposeTraceID(loggingMiddleware(handler, logger))
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

func NewServerImpl(
	cfg config.Config,
	logger *slog.Logger,
	database db.DB,
	temporalClient client.Client,
	k8sReader ctrlclient.Reader,
	opts ...Option,
) *serverImpl {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	nodePoolService := NewNodePoolService(
		logger,
		database,
		cfg.Teleport,
		cfg.SupportedInfraProviders,
		cfg.UseControlPlaneControllerForControlPlane(),
	)

	clusterService := NewClusterService(
		logger,
		database,
		nodePoolService,
		cfg.Teleport,
		cfg.NkeOIDCAuth.Enabled,
		cfg.SupportedInfraProviders,
		cfg.UseControlPlaneControllerForControlPlane(),
	)
	clusterService.fleetOIDC = o.fleetOIDC

	return &serverImpl{
		clusterService:       *clusterService,
		nodePoolService:      *nodePoolService,
		nodeSetService:       *NewNodeSetService(logger, database, temporalClient),
		nodeService:          *NewNodeService(logger, database, cfg.UseControlPlaneControllerForControlPlane()),
		osProfileService:     *NewOSProfileService(logger, k8sReader),
		configProfileService: *NewConfigProfileService(cfg, logger),
	}
}

type serverImpl struct {
	clusterService
	nodePoolService
	nodeSetService
	nodeService
	osProfileService
	configProfileService
}
