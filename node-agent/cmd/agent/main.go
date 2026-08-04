package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	server "github.com/NVIDIA/nke/node-agent/internal/agent"
	"github.com/NVIDIA/nke/node-agent/internal/grpcserver"
)

const (
	DefaultNodeServerListenPort = "15005"
)

// VERSION is set at build time via: -ldflags "-X main.VERSION=<version>"
// Keep it as variable or things are going to break
var VERSION string

// AgentConfig centralizes configuration previously stored in globals.
// It is passed explicitly to functions instead of relying on package-level state.
// TODO:  May want to move this and define it in the agent package, and just
// pass the full config to NewAgent but for now beyond the scope of this commit :)
type AgentConfig struct {
	Version               string
	NodeServerListenPort  string
	MLX5Rescan            bool
	BundleServerURL       string
	IndexFileURL          string
	CniImageRegistry      string
	AuditLogger           *slog.Logger
	ScriptExecutionLogger *slog.Logger
	DisableMTLS           bool
	SPIFFESocketPath      string
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

	slog.Info("executing /usr/local/nke/bin/bootstrap.sh", "kubeVersion", kubeVersion)
	cmd := exec.Command("/usr/local/nke/bin/bootstrap.sh", kubeVersion)

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

	authorizer := tlsconfig.AdaptMatcher(func(clientID spiffeid.ID) error {
		if clientID.TrustDomain().Name() != agentTrustDomain {
			return errors.New("unauthorized: trust domain mismatch")
		}
		if !strings.HasPrefix(clientID.Path(), "/nke-service/") {
			return errors.New("unauthorized: path does not start with /nke-service/")
		}
		return nil
	})

	return tlsconfig.MTLSServerConfig(source, source, authorizer), nil
}

func checkKeylessEntryConfig() (string, error) {
	// Check if the keyless entry configuration file exists
	_, err := os.Stat("/etc/keyless-entry.conf")
	if err != nil && errors.Is(err, os.ErrNotExist) {
		slog.Info("keyless entry configuration file does not exist")
		return "", nil
	}
	if err != nil {
		slog.Error("failed to stat keyless entry configuration file", "err", err)
		return "", err
	}

	// Read the keyless entry configuration file
	file, _ := os.ReadFile("/etc/keyless-entry.conf")
	return string(file), nil
}

// parseKeylessEntryConfig parses keyless entry data into the server state.
func parseKeylessEntryConfig(serverState *server.NodeStatus) {
	uuidCounter := 0
	for _, line := range strings.Split(serverState.DiskEncryptionStatus.KeylessEntryConfig, "\n") {
		if strings.Contains(line, "UUID=") {
			serverState.DiskEncryptionStatus.EncryptedDeviceUUID = strings.Split(line, "UUID=")[1]
			uuidCounter += 1
		}
		if strings.Contains(line, "configured = true") {
			serverState.DiskEncryptionStatus.KeylessEntryConfigured = true
		}

		if strings.Contains(line, "enabled = true") {
			serverState.DiskEncryptionStatus.KeylessEntryEnabled = true
		}
	}
	if uuidCounter > 1 {
		log.Fatal("ERROR: multiple UUIDs found in keyless entry configuration")
	}
}

// newAuditLogInterceptor returns an interceptor that logs audit events using the provided logger.
func newAuditLogInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		method *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		client := "Unavailable"
		errorResponse := "nil"
		clientAddress := "Unavailable"

		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			ua := md.Get("user-agent")
			if len(ua) > 0 {
				client = ua[0]
			}
		}

		m, err := handler(ctx, req)
		if err != nil {
			errorResponse = err.Error()
		}

		p, ok := peer.FromContext(ctx)
		if ok {
			clientAddress = p.Addr.String()
		}

		logger.Info(
			"grpc request received",
			slog.String("method", method.FullMethod),
			slog.String("error", errorResponse),
			slog.String("user-agent", client),
			slog.String("client-address", clientAddress),
		)
		return m, err
	}
}

func startSecureServer(ctx context.Context, cfg AgentConfig, agentServer *server.NodeAgentServer, auditLogger *slog.Logger) {
	serverOptions := []grpc.ServerOption{
		grpc.UnaryInterceptor(newAuditLogInterceptor(auditLogger)),
	}

	if !cfg.DisableMTLS {
		slog.Info("setting up SPIFFE mTLS for gRPC")
		tlsCfg, err := buildSpiffeMtlsConfig(ctx, cfg.SPIFFESocketPath)
		if err != nil {
			log.Fatalf("failed to build SPIFFE mTLS config: %v", err)
		}
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	s := grpc.NewServer(serverOptions...)

	grpcserver.Register(s, agentServer)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", cfg.NodeServerListenPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	agentServer.NodeStatus.ListenStatus = "listening on port " + cfg.NodeServerListenPort
	slog.Info("persisting status with listenStatus update")
	agentServer.Persister.Persist(agentServer.NodeStatus) //nolint errcheck
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

	// check keyless-entry configuration, we want this on every reboot or restart of the service...
	keylessEntryConfig, err := checkKeylessEntryConfig()
	if err != nil {
		log.Fatalf("failed to check keyless entry configuration: %v", err)
	}

	if keylessEntryConfig != "" {
		slog.Info("keyless entry configuration", "config", keylessEntryConfig)
		agentServer.NodeStatus.DiskEncryptionStatus.KeylessEntryConfig = keylessEntryConfig
		parseKeylessEntryConfig(agentServer.NodeStatus)
	}

	cmd := exec.Command("swapoff", "-a")
	_, err = cmd.CombinedOutput()
	if err != nil {
		slog.Error("failed to disable swap", "err", err)
	}
	return agentServer, nil
}

func main() {
	disableMLX5Rescan := flag.Bool("disable-mlx5-rescan", false, "Disable mlx5 rescan (disables the init routine mlx5 reset proc)")
	disableMTLS := flag.Bool("disable-mtls", false, "Disable mTLS / use plaintext gRPC (development only)")
	spiffeSocketPath := flag.String("spiffe-socket-path", "unix:///tmp/spire-agent/public/api.sock", "Path to the SPIFFE workload API socket (--disable-mtls ignores this)")
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	auditLogFileName := flag.String(
		"audit-log-file",
		"/var/log/nke-agent-audit.log",
		"The audit log file name",
	)
	secureServerPort := flag.String("secure-server-port", DefaultNodeServerListenPort, "The port for the gRPC server")
	bundleServerURL := flag.String("bundle-server-url", "https://nke-dev.s3.us-west-2.amazonaws.com/", "Bundle source URL. Supports http://, https://, file://, or oci:// schemes. Use oci:// prefix for OCI registry (e.g., oci://nvcr.io/j7sbcjl3qgta/development)")
	indexFileURL := flag.String("index-file-url", "", "Full URL to custom index file, by default will use <bundleServeruRL>/nke_bundles/ubntu_<version>_index.json")
	cniImageRegistry := flag.String("cni-image-registry", "", "OCI registry prefix for CNI component images (e.g. my-mirror.example.com). When set, passed to the CNI helm chart as --set global.registry=<value>. Leave empty to use chart defaults.")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("NKE Agent Version: %s\n", VERSION)
		os.Exit(0)
	}

	auditFile, err := os.OpenFile(*auditLogFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("failed to open audit log file: %v", err)
	}
	scriptExecLogFile, err := os.OpenFile("/var/log/nke-execute-scripts.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("failed to open script execution log file: %v", err)
	}
	// Create audit logger, script execution logger, and default logger
	auditLogger := slog.New(slog.NewJSONHandler(auditFile, nil))
	scriptExecutionLogger := slog.New(slog.NewJSONHandler(scriptExecLogFile, nil))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Build config from flags and build-time vars
	cfg := AgentConfig{
		Version:               VERSION,
		NodeServerListenPort:  *secureServerPort,
		MLX5Rescan:            !*disableMLX5Rescan,
		BundleServerURL:       *bundleServerURL,
		IndexFileURL:          *indexFileURL,
		CniImageRegistry:      *cniImageRegistry,
		AuditLogger:           auditLogger,
		ScriptExecutionLogger: scriptExecutionLogger,
		DisableMTLS:           *disableMTLS,
		SPIFFESocketPath:      *spiffeSocketPath,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("initializing node agent and setting status")
	agentServer, err := RunNodeStartup(cfg)
	if err != nil {
		slog.Error("failed to run node startup", "err", err)
	} else {
		agentServer.Persister.Persist(agentServer.NodeStatus) // nolint errcheck
	}
	go func() {
		slog.Info("starting server")
		startSecureServer(ctx, cfg, agentServer, cfg.AuditLogger)
	}()

	<-ctx.Done()
}
