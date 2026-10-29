// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	server "github.com/dsx-ai-factory/nvke/agent/internal/agent"
	"github.com/dsx-ai-factory/nvke/agent/internal/externalmtls"
	"github.com/dsx-ai-factory/nvke/agent/internal/grpcserver"
	"github.com/dsx-ai-factory/nvke/agent/internal/tlsjwt"
)

const (
	DefaultNodeServerListenPort = "15005"

	// authModeSPIFFEMTLS serves gRPC over SPIFFE mTLS alone: the agent's X.509
	// SVID is its identity to the controller.
	authModeSPIFFEMTLS = "spiffe-mtls"
	// authModeTLSJWT adds the pre-gRPC authentication exchange on top of the
	// secure base transport. The agent sends a DSX identity token once the
	// handshake has authenticated the controller.
	authModeTLSJWT = "tls-jwt"

	// defaultDSXIdentityTimeout bounds one token-exchange call. It sits well
	// inside tlsjwt.HandshakeTimeout so a slow exchange fails the exchange
	// rather than the whole handshake deadline.
	defaultDSXIdentityTimeout = 10 * time.Second
)

// VERSION is set at build time via: -ldflags "-X main.VERSION=<version>"
// Keep it as variable or things are going to break
//
// "dev", not the zero value, so a build with no -X says so. Empty, it printed
// "NVKE Agent Version: " and registered an empty version, which reads as a
// display bug instead of a missing stamp.
var VERSION = "dev"

// envOr returns the value of the named environment variable, or fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// registerControllerClientCABundleFlag registers --nvke-controller-client-ca-bundle,
// which defaults to NVKE_CONTROLLER_CLIENT_CA_BUNDLE.
func registerControllerClientCABundleFlag(fs *flag.FlagSet) *string {
	return fs.String(
		"nvke-controller-client-ca-bundle",
		envOr("NVKE_CONTROLLER_CLIENT_CA_BUNDLE", ""),
		fmt.Sprintf("PEM CA bundle verifying the controller's client certificate. Setting it selects external mTLS "+
			"instead of SPIFFE mTLS and requires --grpc-auth-mode=%s. The controller's certificate must also carry "+
			"the URI SAN %s. Also set via NVKE_CONTROLLER_CLIENT_CA_BUNDLE", authModeTLSJWT, externalmtls.ControllerURI),
	)
}

// AgentConfig centralizes configuration previously stored in globals.
// It is passed explicitly to functions instead of relying on package-level state.
// TODO:  May want to move this and define it in the agent package, and just
// pass the full config to NewAgent but for now beyond the scope of this commit :)
type AgentConfig struct {
	Version                  string
	NodeServerListenPort     string
	MLX5Rescan               bool
	BundleServerURL          string
	IndexFileURL             string
	CniImageRegistry         string
	AuditLogger              *slog.Logger
	ScriptExecutionLogger    *slog.Logger
	DisableMTLS              bool
	SPIFFESocketPath         string
	GRPCAuthMode             string
	DSXIdentityExchangeURL   string
	DSXIdentityCABundle      string
	DSXIdentityTimeout       time.Duration
	ControllerClientCABundle string
	ControllerAudience       string
}

// validateAuthConfig rejects authentication settings that cannot work, so a
// misconfigured agent fails at startup with a readable message instead of
// failing every handshake.
func validateAuthConfig(cfg AgentConfig) error {
	if cfg.ControllerAudience != "" && cfg.ControllerAudience != tlsjwt.ControllerAudience && cfg.ControllerAudience != tlsjwt.LegacyControllerAudience {
		return fmt.Errorf("--controller-audience must be %q or %q", tlsjwt.ControllerAudience, tlsjwt.LegacyControllerAudience)
	}
	if cfg.ControllerClientCABundle != "" {
		if cfg.DisableMTLS {
			return errors.New("--nvke-controller-client-ca-bundle cannot be combined with --disable-mtls: " +
				"there is no TLS handshake in which to verify the controller certificate")
		}
		// The controller does not verify the self-signed certificate the agent
		// serves in this mode, so without the identity token nothing
		// authenticates the agent to the controller.
		if cfg.GRPCAuthMode != authModeTLSJWT {
			return fmt.Errorf("--nvke-controller-client-ca-bundle requires --grpc-auth-mode=%s: "+
				"the controller cannot otherwise authenticate the agent", authModeTLSJWT)
		}
	}

	switch cfg.GRPCAuthMode {
	case authModeSPIFFEMTLS:
		return nil
	case authModeTLSJWT:
		if cfg.DisableMTLS {
			return fmt.Errorf("--grpc-auth-mode=%s cannot be combined with --disable-mtls: "+
				"the agent must authenticate the controller before it sends an identity token", authModeTLSJWT)
		}
		if cfg.DSXIdentityExchangeURL == "" {
			return fmt.Errorf("--grpc-auth-mode=%s requires --dsx-identity-exchange-url", authModeTLSJWT)
		}
		return nil
	default:
		return fmt.Errorf("--grpc-auth-mode must be %q or %q, got %q",
			authModeSPIFFEMTLS, authModeTLSJWT, cfg.GRPCAuthMode)
	}
}

func Bootstrap(kubeVersion string) error {
	versionResponse, err := GetVersionInfo()
	if err == nil {
		slog.Info("existing kubeadm version info", "version", versionResponse)
		return nil
	}

	if !strings.Contains(err.Error(), "not found") {
		slog.Error("kubeadm version not found")
		return err
	}

	slog.Info("executing /usr/local/nvke/bin/bootstrap.sh", "kubeVersion", kubeVersion)
	cmd := exec.Command("/usr/local/nvke/bin/bootstrap.sh", kubeVersion)

	output, err := cmd.CombinedOutput()
	if err != nil {
		bootstrapErr := fmt.Errorf("kubeadm bootstrap failed.  Error: %v, Output: %s", err, output)
		slog.Error("kubeadm bootstrap failed", "err", bootstrapErr)
		return bootstrapErr
	}

	// We need to rotate any state file that exists, since kubeadm not-found we start over...
	slog.Info("kubeadm installed successfully, node is initialized and ready to join a cluster.")
	return nil
}

func GetVersionInfo() (string, error) {
	// Make sure kubeadm exists...
	cmd := exec.Command("kubeadm", "version", "-o", "short")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "not found") {
			return "", errors.New("kubeadm not found")
		}
		return "", err
	}
	version := bytes.TrimSpace(out)
	return string(version), nil
}

// callerPathPrefixes are the SPIFFE ID paths permitted to drive this agent, matched
// as prefixes: "/nke-service/" does not admit "/nke-service-evil/". Both name the
// same callers -- NVKE is migrating its IDs onto the tenant grammar, and agents have
// to accept the old form until the ClusterSPIFFEID template changes and the new one
// after. Drop "/nke-service/" once nothing issues it.
var callerPathPrefixes = []string{
	"/tenant/nvke/service/",
	"/nvke-service/",
	"/nke-service/",
	"/tenant/nke/service/",
}

// authorizeClient returns the matcher deciding which peer SPIFFE IDs may call this
// agent: same trust domain, and a path under callerPathPrefixes.
func authorizeClient(agentTrustDomain string) func(spiffeid.ID) error {
	return func(clientID spiffeid.ID) error {
		if clientID.TrustDomain().Name() != agentTrustDomain {
			return errors.New("unauthorized: trust domain mismatch")
		}
		for _, prefix := range callerPathPrefixes {
			if strings.HasPrefix(clientID.Path(), prefix) {
				return nil
			}
		}
		return fmt.Errorf("unauthorized: path %q does not start with one of %s",
			clientID.Path(), strings.Join(callerPathPrefixes, ", "))
	}
}

// buildServerTLSConfig selects how the agent authenticates the controller.
// A controller client CA bundle means the controller presents an externally
// issued certificate; otherwise the controller is a SPIFFE workload.
func buildServerTLSConfig(ctx context.Context, cfg AgentConfig) (*tls.Config, error) {
	if cfg.ControllerClientCABundle != "" {
		slog.Info("setting up external mTLS for gRPC", "controller_client_ca_bundle", cfg.ControllerClientCABundle)
		return externalmtls.ServerConfig(cfg.ControllerClientCABundle)
	}

	slog.Info("setting up SPIFFE mTLS for gRPC")
	return buildSpiffeMtlsConfig(ctx, cfg.SPIFFESocketPath)
}

func buildSpiffeMtlsConfig(ctx context.Context, socketPath string) (*tls.Config, error) {
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)))
	if err != nil {
		return nil, fmt.Errorf("failed to create x509 source: %w", err)
	}

	svid, err := source.GetX509SVID()
	if err != nil {
		return nil, fmt.Errorf("failed to get x509 SVID: %w", err)
	}

	agentTrustDomain := svid.ID.TrustDomain().Name()

	authorizer := tlsconfig.AdaptMatcher(authorizeClient(agentTrustDomain))

	return tlsconfig.MTLSServerConfig(source, source, authorizer), nil
}

// buildTLSJWTCredentials wraps base with the pre-gRPC authentication exchange.
// It builds the token source but does not mint a token: the first connection
// does that, so an unreachable SPIRE or DSX Identity leaves the agent listening
// and failing handshakes rather than refusing to start.
func buildTLSJWTCredentials(
	ctx context.Context,
	cfg AgentConfig,
	base credentials.TransportCredentials,
) (credentials.TransportCredentials, error) {
	jwtSource, err := workloadapi.NewJWTSource(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(cfg.SPIFFESocketPath)))
	if err != nil {
		return nil, fmt.Errorf("failed to create jwt-svid source: %w", err)
	}

	exchanger, err := tlsjwt.NewExchangeClient(cfg.DSXIdentityExchangeURL, cfg.DSXIdentityCABundle, cfg.DSXIdentityTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to create dsx identity exchange client: %w", err)
	}

	audience := cfg.ControllerAudience
	if audience == "" {
		audience = tlsjwt.LegacyControllerAudience
	}
	tokens, err := tlsjwt.NewCachingTokenSource(tlsjwt.WorkloadAPISVIDs(jwtSource), exchanger, audience)
	if err != nil {
		return nil, fmt.Errorf("failed to create node-agent token source: %w", err)
	}

	return tlsjwt.NewServerCredentials(base, tokens, slog.Default())
}

func logAuditEvent(logger *slog.Logger, ctx context.Context, method string, startedAt time.Time, err error) {
	client := "Unavailable"
	clientAddress := "Unavailable"
	errorResponse := "nil"

	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if userAgents := md.Get("user-agent"); len(userAgents) > 0 {
			client = userAgents[0]
		}
	}
	if p, ok := peer.FromContext(ctx); ok {
		clientAddress = p.Addr.String()
	}
	if err != nil {
		errorResponse = err.Error()
	}

	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("error", errorResponse),
		slog.String("user-agent", client),
		slog.String("client-address", clientAddress),
		slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
	}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		attrs = append(attrs,
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "grpc request received", attrs...)
}

// newAuditLogUnaryInterceptor returns an interceptor that logs unary audit events using the provided logger.
func newAuditLogUnaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		method *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		startedAt := time.Now()
		response, err := handler(ctx, req)
		logAuditEvent(logger, ctx, method.FullMethod, startedAt, err)
		return response, err
	}
}

// newAuditLogStreamInterceptor returns an interceptor that logs streaming audit events using the provided logger.
func newAuditLogStreamInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		startedAt := time.Now()
		err := handler(server, stream)
		logAuditEvent(logger, stream.Context(), info.FullMethod, startedAt, err)
		return err
	}
}

func newGRPCServerOptions(auditLogger *slog.Logger) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithPropagators(propagation.TraceContext{}),
			otelgrpc.WithTracerProvider(noop.NewTracerProvider()),
		)),
		grpc.UnaryInterceptor(newAuditLogUnaryInterceptor(auditLogger)),
		grpc.StreamInterceptor(newAuditLogStreamInterceptor(auditLogger)),
	}
}

func startSecureServer(ctx context.Context, cfg AgentConfig, agentServer *server.NodeAgentServer, auditLogger *slog.Logger) {
	serverOptions := newGRPCServerOptions(auditLogger)

	if !cfg.DisableMTLS {
		tlsCfg, err := buildServerTLSConfig(ctx, cfg)
		if err != nil {
			slog.Error("failed to build the gRPC server TLS config", "err", err)
			os.Exit(1)
		}

		var transportCredentials credentials.TransportCredentials = credentials.NewTLS(tlsCfg)
		if cfg.GRPCAuthMode == authModeTLSJWT {
			slog.Info("adding TLS/JWT authentication to gRPC", "exchange_endpoint", cfg.DSXIdentityExchangeURL)
			transportCredentials, err = buildTLSJWTCredentials(ctx, cfg, transportCredentials)
			if err != nil {
				slog.Error("failed to build TLS/JWT credentials", "err", err)
				os.Exit(1)
			}
		}
		serverOptions = append(serverOptions, grpc.Creds(transportCredentials))
	}

	s := grpc.NewServer(serverOptions...)

	grpcserver.Register(s, agentServer)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", cfg.NodeServerListenPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	agentServer.NodeStatus.ListenStatus = "listening on port " + cfg.NodeServerListenPort
	slog.Info("persisting status with listenStatus update")
	agentServer.Persister.Persist(agentServer.NodeStatus) //nolint:errcheck
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
	agentServer.Persister.Persist(agentServer.NodeStatus) //nolint:errcheck
}

func initializeNodeServer(cfg AgentConfig) *server.NodeAgentServer {
	slog.Info("running initializeNodeServer with the following config",
		"CONFIG",
		cfg)
	agentServer := server.NewNodeAgentServer(cfg.MLX5Rescan, cfg.BundleServerURL, cfg.IndexFileURL, cfg.CniImageRegistry)
	agentServer.ScriptExecutionLogger = cfg.ScriptExecutionLogger
	// The SPIRE RPCs read SVIDs from the socket whether or not the gRPC server uses mTLS.
	agentServer.SPIFFESocketPath = cfg.SPIFFESocketPath
	agentServer.NodeStatus.ListenStatus = "pending"
	if err := agentServer.Persister.Persist(agentServer.NodeStatus); err != nil {
		slog.Error("failed to persist initial status", "err", err)
	}

	agentServer.NodeStatus.AgentVersion = cfg.Version
	return agentServer
}

func RunNodeStartup(cfg AgentConfig) (*server.NodeAgentServer, error) {
	slog.Info("running NodeStartup...")
	agentServer := initializeNodeServer(cfg)

	cmd := exec.Command("swapoff", "-a")
	_, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("failed to disable swap", "err", err)
	}
	return agentServer, nil
}

type fanoutOutput struct {
	name   string
	writer io.Writer
}

type fanoutWriter struct {
	outputs     []fanoutOutput
	diagnostics *slog.Logger
}

// Write attempts every output even if an earlier one fails. Returning the full
// input length prevents callers from retrying and duplicating a record in the
// outputs that succeeded; the joined error still reports any failed copies.
func (w fanoutWriter) Write(p []byte) (int, error) {
	var errs []error
	for i, output := range w.outputs {
		n, err := output.writer.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			err = fmt.Errorf("write %s audit output: %w", output.name, err)
			errs = append(errs, err)
			w.diagnostics.Error("failed to write audit output", "output", output.name, "output_index", i, "err", err)
		}
	}

	return len(p), errors.Join(errs...)
}

// newAuditLogger mirrors audit records to their durable file and the service
// output collected by journald.
func newAuditLogger(fileOutput, serviceOutput io.Writer, diagnostics *slog.Logger) *slog.Logger {
	return slog.New(slog.NewJSONHandler(fanoutWriter{
		outputs: []fanoutOutput{
			{name: "file", writer: fileOutput},
			{name: "service", writer: serviceOutput},
		},
		diagnostics: diagnostics,
	}, nil))
}

func main() {
	disableMLX5Rescan := flag.Bool("disable-mlx5-rescan", false, "Disable mlx5 rescan (disables the init routine mlx5 reset proc)")
	disableMTLS := flag.Bool("disable-mtls", false, "Disable mTLS / use plaintext gRPC (development only)")
	spiffeSocketPath := flag.String("spiffe-socket-path", "unix:///tmp/spire-agent/public/api.sock", "Path to the SPIFFE workload API socket, used for the gRPC server's mTLS unless --disable-mtls is set and always for the SPIRE agent RPCs")
	grpcAuthMode := flag.String(
		"grpc-auth-mode",
		authModeSPIFFEMTLS,
		fmt.Sprintf("gRPC authentication mode: %q serves SPIFFE mTLS alone, %q also sends a DSX identity token "+
			"to the controller before gRPC starts", authModeSPIFFEMTLS, authModeTLSJWT),
	)
	dsxIdentityExchangeURL := flag.String(
		"dsx-identity-exchange-url",
		envOr("NKE_DSX_IDENTITY_EXCHANGE_URL", ""),
		"DSX Identity token-exchange endpoint, required by --grpc-auth-mode=tls-jwt. Also set via NKE_DSX_IDENTITY_EXCHANGE_URL. "+
			"Example: https://dsx-identity.example.com/v1/exchange",
	)
	dsxIdentityCABundle := flag.String(
		"dsx-identity-ca-bundle",
		envOr("NKE_DSX_IDENTITY_CA_BUNDLE", ""),
		"PEM CA bundle authenticating the DSX Identity endpoint. When empty, the system trust store is used. "+
			"Also set via NKE_DSX_IDENTITY_CA_BUNDLE",
	)
	dsxIdentityTimeout := flag.Duration(
		"dsx-identity-timeout",
		defaultDSXIdentityTimeout,
		"Timeout for one DSX Identity token-exchange request",
	)
	controllerClientCABundle := registerControllerClientCABundleFlag(flag.CommandLine)
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	auditLogFileName := flag.String(
		"audit-log-file",
		"/var/log/nvke-agent-audit.log",
		"The audit log file name",
	)
	secureServerPort := flag.String("secure-server-port", DefaultNodeServerListenPort, "The port for the gRPC server")
	bundleServerURL := flag.String(
		"bundle-server-url",
		envOr("NKE_BUNDLE_SERVER_URL", ""),
		"Optional bundle source URL for InstallBundle/InstallCollection(.json). Supports http://, https://, file://, or oci://. Also set via NKE_BUNDLE_SERVER_URL. Prefer apt repos when unset. Example: https://artifacts.example.com/ or oci://registry.example.com/nvke/bundles",
	)
	indexFileURL := flag.String(
		"index-file-url",
		envOr("NKE_BUNDLE_INDEX_URL", ""),
		"Full URL to a custom index file. When empty, derived as <bundle-server-url>/nke_bundles/ubuntu_<version>_index.json. Also set via NKE_BUNDLE_INDEX_URL",
	)
	cniImageRegistry := flag.String(
		"cni-image-registry",
		envOr("NKE_CNI_IMAGE_REGISTRY", ""),
		"OCI registry prefix for CNI component images (e.g. registry.example.com/nvke). When set, passed to the CNI helm chart as --set global.registry=<value>. Also set via NKE_CNI_IMAGE_REGISTRY",
	)
	controllerAudience := flag.String("controller-audience", tlsjwt.LegacyControllerAudience, "JWT audience: select nvke-controller once the controller accepts the renamed contract")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("NVKE Agent Version: %s\n", VERSION)
		os.Exit(0)
	}

	auditFile, err := os.OpenFile(*auditLogFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed to open audit log file: %v", err)
	}
	scriptExecLogFile, err := os.OpenFile("/var/log/nvke-execute-scripts.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed to open script execution log file: %v", err)
	}
	// Create audit logger, script execution logger, and default logger
	auditLogger := newAuditLogger(auditFile, os.Stdout, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	scriptExecutionLogger := slog.New(slog.NewJSONHandler(scriptExecLogFile, nil))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Build config from flags and build-time vars
	cfg := AgentConfig{
		Version:                  VERSION,
		NodeServerListenPort:     *secureServerPort,
		MLX5Rescan:               !*disableMLX5Rescan,
		BundleServerURL:          *bundleServerURL,
		IndexFileURL:             *indexFileURL,
		CniImageRegistry:         *cniImageRegistry,
		AuditLogger:              auditLogger,
		ScriptExecutionLogger:    scriptExecutionLogger,
		DisableMTLS:              *disableMTLS,
		SPIFFESocketPath:         *spiffeSocketPath,
		GRPCAuthMode:             *grpcAuthMode,
		DSXIdentityExchangeURL:   *dsxIdentityExchangeURL,
		DSXIdentityCABundle:      *dsxIdentityCABundle,
		DSXIdentityTimeout:       *dsxIdentityTimeout,
		ControllerClientCABundle: *controllerClientCABundle,
		ControllerAudience:       *controllerAudience,
	}

	if err := validateAuthConfig(cfg); err != nil {
		slog.Error("invalid authentication configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("initializing node agent and setting status")
	agentServer, err := RunNodeStartup(cfg)
	if err != nil {
		slog.Error("failed to run node startup", "err", err)
	} else {
		agentServer.Persister.Persist(agentServer.NodeStatus) //nolint:errcheck
	}
	go func() {
		slog.Info("starting server")
		startSecureServer(ctx, cfg, agentServer, cfg.AuditLogger)
	}()

	<-ctx.Done()
}
