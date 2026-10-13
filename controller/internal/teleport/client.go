// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/trace"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

type JoinMethod string

const (
	JoinMethodTPM JoinMethod = "tpm"

	// DefaultRetryMaxAttempts is the number of times to retry failed operations.
	DefaultRetryMaxAttempts = 3
	// DefaultRetryInitialBackoff is the initial backoff duration between retries.
	DefaultRetryInitialBackoff = 1 * time.Second
	// DefaultConnectionTimeout is the timeout for establishing a connection.
	DefaultConnectionTimeout = 30 * time.Second
)

var (
	ErrTeleportDisabled      = errors.New("teleport integration is disabled")
	ErrIdentityFileNotSet    = errors.New("teleport.identityFile is not set")
	ErrProxyAddressNotSet    = errors.New("teleport.proxyAddress is not set")
	ErrUnsupportedJoinMethod = errors.New("unsupported join method")
)

// TokenSpec is a marker interface for join-method-specific configuration.
// Each join method (TPM, IAM, GitLab, etc.) has its own config type that implements this.
type TokenSpec interface {
	tokenSpec() // unexported to prevent external implementations
}

// ProvisionToken represents a Teleport provision token for node/service joining.
type ProvisionToken struct {
	Name       string            // Unique identifier for the token
	Roles      []string          // System roles granted (e.g., "Node", "Kube", "Bot")
	Expiry     time.Time         // When the token expires (zero = no expiry)
	Labels     map[string]string // Key-value metadata
	JoinMethod JoinMethod        // How resources join using this token
	BotName    string            // Set when this token is for a Machine ID bot
	Spec       TokenSpec         // Join-method-specific config (e.g., *TPMConfig)
}

// ---------------------------------------------------------------------------
// TPM Join Method
// ---------------------------------------------------------------------------

// TPMConfig contains configuration for the TPM join method.
type TPMConfig struct {
	Allow            []TPMRule // TPM attestation rules (at least one required)
	EKCertAllowedCAs []string  // CA certificates that validate TPM EKCerts
}

func (*TPMConfig) tokenSpec() {} // implements TokenSpec

// TPMRule represents a rule for TPM join token matching.
type TPMRule struct {
	Description         string // Human-readable description
	EKPublicHash        string // SHA256 hash of the EK public key
	EKCertificateSerial string // Serial number of the EK certificate
}

// Node represents a Teleport node (SSH server).
type Node struct {
	Name      string
	Hostname  string
	Addr      string
	Labels    map[string]string
	Namespace string
}

// ListNodesOption configures a ListNodes call.
type ListNodesOption func(*listNodesOptions)

type listNodesOptions struct {
	extraLabels map[string]string
}

// WithLabels adds additional label filters to a ListNodes request.
// These are merged with the default OwnershipLabels; ownership labels take precedence.
func WithLabels(labels map[string]string) ListNodesOption {
	return func(o *listNodesOptions) {
		o.extraLabels = labels
	}
}

// Client defines the interface for Teleport API operations needed by the controller.
// All token operations enforce ownership - the client will only interact with
// tokens that have the "managed-by: nke-cloud-workflow-engine" label.
type Client interface {
	Close() error

	// Token operations - all methods enforce ownership labels.
	// UpsertToken automatically adds ownership labels.
	// GetToken, DeleteToken, ListTokens only operate on controller-owned tokens.
	UpsertToken(ctx context.Context, token ProvisionToken) error
	GetToken(ctx context.Context, name string) (*ProvisionToken, error)
	DeleteToken(ctx context.Context, name string) error
	ListTokens(ctx context.Context) ([]ProvisionToken, error)

	GetNode(ctx context.Context, namespace, name string) (*Node, error)
	DeleteNode(ctx context.Context, namespace, name string) error
	ListNodes(ctx context.Context, namespace string, opts ...ListNodesOption) ([]Node, error)
}

type clientImpl struct {
	proxyAddress        string
	credentialsPath     string
	managedByLabelValue string
	logger              *slog.Logger

	mu        sync.RWMutex
	apiClient *client.Client
}

type ClientOption func(*clientImpl)

func WithLogger(logger *slog.Logger) ClientOption {
	return func(c *clientImpl) {
		c.logger = logger
	}
}

func NewClient(ctx context.Context, cfg config.TeleportConfig, opts ...ClientOption) (Client, error) {
	if !cfg.Enabled {
		return nil, ErrTeleportDisabled
	}

	if cfg.IdentityFile == "" {
		return nil, ErrIdentityFileNotSet
	}

	if cfg.ProxyAddress == "" {
		return nil, ErrProxyAddressNotSet
	}

	c := &clientImpl{
		proxyAddress:        cfg.ProxyAddress,
		credentialsPath:     cfg.IdentityFile,
		managedByLabelValue: cfg.ManagedByLabelValue,
		logger:              slog.Default(),
	}

	for _, opt := range opts {
		opt(c)
	}

	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *clientImpl) connect(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, DefaultConnectionTimeout)
	defer cancel()

	creds := client.LoadIdentityFile(c.credentialsPath)

	apiClient, err := client.New(connectCtx, client.Config{
		Addrs:       []string{c.proxyAddress},
		Credentials: []client.Credentials{creds},
	})
	if err != nil {
		return fmt.Errorf("failed to create teleport client: %w", err)
	}

	c.mu.Lock()
	if c.apiClient != nil {
		c.apiClient.Close()
	}
	c.apiClient = apiClient
	c.mu.Unlock()

	c.logger.Info("teleport client connected", "proxy_address", c.proxyAddress)

	return nil
}

func (c *clientImpl) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.apiClient == nil {
		return nil
	}

	err := c.apiClient.Close()
	c.apiClient = nil
	return err
}

func (c *clientImpl) UpsertToken(ctx context.Context, token ProvisionToken) error {
	// Inject ownership labels - these are always added/overwritten
	token.Labels = MergeLabels(token.Labels, c.managedByLabelValue)

	return c.withRetry(ctx, "UpsertToken", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		teleportToken, err := c.toTeleportToken(token)
		if err != nil {
			return fmt.Errorf("failed to convert token: %w", err)
		}

		if err := apiClient.UpsertToken(ctx, teleportToken); err != nil {
			return err
		}

		c.logger.Debug("upserted token",
			"name", token.Name,
			"join_method", token.JoinMethod,
			LabelManagedBy, token.Labels[LabelManagedBy],
		)
		return nil
	})
}

func (c *clientImpl) GetToken(ctx context.Context, name string) (*ProvisionToken, error) {
	var result *ProvisionToken
	err := c.withRetry(ctx, "GetToken", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		teleportToken, err := apiClient.GetToken(ctx, name)
		if err != nil {
			return err
		}

		result = fromTeleportToken(teleportToken)

		// Verify ownership - refuse to return tokens not managed by the controller
		if !IsOwnedByController(result.Labels, c.managedByLabelValue) {
			c.logger.Warn("attempted to access token not owned by the controller",
				"name", name,
				LabelManagedBy, result.Labels[LabelManagedBy],
			)
			return ErrNotOwnedByController
		}

		c.logger.Debug("retrieved token", "name", name)
		return nil
	})

	return result, err
}

func (c *clientImpl) DeleteToken(ctx context.Context, name string) error {
	// First verify ownership by fetching the token
	token, err := c.GetToken(ctx, name)
	if err != nil {
		return err
	}

	// GetToken already verified ownership, but double-check
	if !IsOwnedByController(token.Labels, c.managedByLabelValue) {
		c.logger.Warn("attempted to delete token not owned by the controller",
			"name", name,
			LabelManagedBy, token.Labels[LabelManagedBy],
		)
		return ErrNotOwnedByController
	}

	return c.withRetry(ctx, "DeleteToken", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		if err := apiClient.DeleteToken(ctx, name); err != nil {
			return err
		}

		c.logger.Debug("deleted token", "name", name)
		return nil
	})
}

func (c *clientImpl) ListTokens(ctx context.Context) ([]ProvisionToken, error) {
	var result []ProvisionToken
	err := c.withRetry(ctx, "ListTokens", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		teleportTokens, err := apiClient.GetTokens(ctx)
		if err != nil {
			return err
		}

		// Filter to only controller-owned tokens
		result = make([]ProvisionToken, 0)
		totalCount := 0
		for _, tt := range teleportTokens {
			totalCount++
			token := fromTeleportToken(tt)
			if IsOwnedByController(token.Labels, c.managedByLabelValue) {
				result = append(result, *token)
			}
		}

		c.logger.Debug("listed tokens", "total", totalCount, "owned", len(result))
		return nil
	})

	return result, err
}

func (c *clientImpl) GetNode(ctx context.Context, namespace, name string) (*Node, error) {
	var result *Node
	err := c.withRetry(ctx, "GetNode", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		teleportNode, err := apiClient.GetNode(ctx, namespace, name)
		if err != nil {
			return err
		}

		result = fromTeleportNode(teleportNode)
		c.logger.Debug("retrieved node", "namespace", namespace, "name", name)
		return nil
	})

	return result, err
}

func (c *clientImpl) DeleteNode(ctx context.Context, namespace, name string) error {
	return c.withRetry(ctx, "DeleteNode", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		if err := apiClient.DeleteNode(ctx, namespace, name); err != nil {
			return err
		}

		c.logger.Debug("deleted node", "namespace", namespace, "name", name)
		return nil
	})
}

func (c *clientImpl) ListNodes(ctx context.Context, namespace string, opts ...ListNodesOption) ([]Node, error) {
	var options listNodesOptions
	for _, opt := range opts {
		opt(&options)
	}

	// Merge caller-provided labels with ownership labels. Ownership labels take precedence.
	labels := make(map[string]string)
	for k, v := range options.extraLabels {
		labels[k] = v
	}
	for k, v := range OwnershipLabels(c.managedByLabelValue) {
		labels[k] = v
	}

	var result []Node
	err := c.withRetry(ctx, "ListNodes", func() error {
		c.mu.RLock()
		apiClient := c.apiClient
		c.mu.RUnlock()

		// Use GetAllResources for paginated listing with server-side label filtering.
		// This handles pagination automatically and downsizes pages on LimitExceeded errors.
		req := &proto.ListResourcesRequest{
			ResourceType: types.KindNode,
			Namespace:    namespace,
			Limit:        defaults.DefaultChunkSize,
			Labels:       labels,
		}

		teleportNodes, err := client.GetAllResources[types.Server](ctx, apiClient, req)
		if err != nil {
			return err
		}

		result = make([]Node, 0, len(teleportNodes))
		for _, tn := range teleportNodes {
			result = append(result, *fromTeleportNode(tn))
		}

		c.logger.Debug("listed nodes", "namespace", namespace, "count", len(result), "labels", labels)
		return nil
	})

	return result, err
}

func (c *clientImpl) toTeleportToken(token ProvisionToken) (types.ProvisionToken, error) {
	roles := make(types.SystemRoles, 0, len(token.Roles))
	for _, r := range token.Roles {
		roles = append(roles, types.SystemRole(r))
	}

	spec := types.ProvisionTokenSpecV2{
		Roles:      roles,
		JoinMethod: types.JoinMethod(token.JoinMethod),
		BotName:    token.BotName,
	}

	// Configure join-method-specific settings.
	// To add a new join method: add a case here and implement toTeleport*Config.
	switch token.JoinMethod {
	case JoinMethodTPM:
		tpmCfg, ok := token.Spec.(*TPMConfig)
		if !ok || tpmCfg == nil {
			return nil, fmt.Errorf("TPM join method requires *TPMConfig in Spec field")
		}
		if len(tpmCfg.Allow) == 0 {
			return nil, fmt.Errorf("TPM join method requires at least one Allow rule")
		}
		spec.TPM = toTeleportTPMConfig(tpmCfg)

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedJoinMethod, token.JoinMethod)
	}

	teleportToken, err := types.NewProvisionTokenFromSpec(token.Name, token.Expiry, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to create provision token: %w", err)
	}

	if len(token.Labels) > 0 {
		metadata := teleportToken.GetMetadata()
		metadata.Labels = token.Labels
		teleportToken.SetMetadata(metadata)
	}

	return teleportToken, nil
}

func toTeleportTPMConfig(cfg *TPMConfig) *types.ProvisionTokenSpecV2TPM {
	tpm := &types.ProvisionTokenSpecV2TPM{
		EKCertAllowedCAs: cfg.EKCertAllowedCAs,
	}

	if len(cfg.Allow) > 0 {
		tpm.Allow = make([]*types.ProvisionTokenSpecV2TPM_Rule, 0, len(cfg.Allow))
		for _, rule := range cfg.Allow {
			tpm.Allow = append(tpm.Allow, &types.ProvisionTokenSpecV2TPM_Rule{
				Description:         rule.Description,
				EKPublicHash:        rule.EKPublicHash,
				EKCertificateSerial: rule.EKCertificateSerial,
			})
		}
	}

	return tpm
}

func fromTeleportToken(tt types.ProvisionToken) *ProvisionToken {
	roles := make([]string, 0, len(tt.GetRoles()))
	for _, r := range tt.GetRoles() {
		roles = append(roles, string(r))
	}

	labels := tt.GetMetadata().Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	token := &ProvisionToken{
		Name:       tt.GetName(),
		Roles:      roles,
		Expiry:     tt.Expiry(),
		Labels:     labels,
		JoinMethod: JoinMethod(tt.GetJoinMethod()),
		BotName:    tt.GetBotName(),
	}

	// Extract join-method-specific configuration.
	// To add a new join method: add a case here and implement fromTeleport*Config.
	if v2, ok := tt.(*types.ProvisionTokenV2); ok {
		switch token.JoinMethod {
		case JoinMethodTPM:
			if v2.Spec.TPM != nil {
				token.Spec = fromTeleportTPMConfig(v2.Spec.TPM)
			}

		default:
			// Unknown/unsupported join method - leave Spec nil
		}
	}

	return token
}

func fromTeleportTPMConfig(tpm *types.ProvisionTokenSpecV2TPM) *TPMConfig {
	cfg := &TPMConfig{
		EKCertAllowedCAs: tpm.EKCertAllowedCAs,
	}

	if len(tpm.Allow) > 0 {
		cfg.Allow = make([]TPMRule, 0, len(tpm.Allow))
		for _, rule := range tpm.Allow {
			cfg.Allow = append(cfg.Allow, TPMRule{
				Description:         rule.Description,
				EKPublicHash:        rule.EKPublicHash,
				EKCertificateSerial: rule.EKCertificateSerial,
			})
		}
	}

	return cfg
}

func fromTeleportNode(tn types.Server) *Node {
	labels := tn.GetAllLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	return &Node{
		Name:      tn.GetName(),
		Hostname:  tn.GetHostname(),
		Addr:      tn.GetAddr(),
		Labels:    labels,
		Namespace: tn.GetNamespace(),
	}
}

// RetryExhaustedError wraps the last error from a retry sequence, indicating
// all retry attempts failed with transient errors.
type RetryExhaustedError struct {
	Operation   string
	Attempts    int
	LastErr     error
	IsTransient bool // true if all failures were transient (network, timeout, etc.)
}

func (e *RetryExhaustedError) Error() string {
	if e.IsTransient {
		return fmt.Sprintf("%s failed after %d attempts (transient): %v", e.Operation, e.Attempts, e.LastErr)
	}
	return fmt.Sprintf("%s failed after %d attempts: %v", e.Operation, e.Attempts, e.LastErr)
}

func (e *RetryExhaustedError) Unwrap() error {
	return e.LastErr
}

// withRetry executes an operation with exponential backoff retry logic.
// On retryable errors, it reconnects to reload credentials (tbot refreshes the identity file).
// Returns the error directly for non-retryable errors, or RetryExhaustedError after all attempts fail.
func (c *clientImpl) withRetry(ctx context.Context, operation string, fn func() error) error {
	const maxBackoff = 10 * time.Second
	var lastErr error

	for attempt := range DefaultRetryMaxAttempts {
		if attempt > 0 {
			backoff := DefaultRetryInitialBackoff * time.Duration(1<<(attempt-1))
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			// Add jitter: +/- 25% of backoff to prevent thundering herd
			//nolint:gosec // G404: math/rand is fine for jitter, not security-sensitive
			jitter := time.Duration(rand.Int64N(int64(backoff / 2)))
			backoff = backoff - (backoff / 4) + jitter

			c.logger.Debug("retrying teleport operation",
				"operation", operation,
				"attempt", attempt+1,
				"backoff", backoff,
			)

			select {
			case <-ctx.Done():
				return fmt.Errorf("%s canceled: %w", operation, ctx.Err())
			case <-time.After(backoff):
			}

			// Reconnect before retry to reload credentials from identity file
			if err := c.connect(ctx); err != nil {
				c.logger.Warn("failed to reconnect before retry", "error", err)
			}
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		// Non-retryable errors return immediately without wrapping
		if isNonRetryableError(lastErr) {
			return lastErr
		}

		c.logger.Warn("teleport operation failed, will retry",
			"operation", operation,
			"error", lastErr,
			"attempt", attempt+1,
		)
	}

	return &RetryExhaustedError{
		Operation:   operation,
		Attempts:    DefaultRetryMaxAttempts,
		LastErr:     lastErr,
		IsTransient: true, // We only get here if all errors were retryable (transient)
	}
}

// isNonRetryableError returns true for errors that should not be retried.
func isNonRetryableError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// NotFound, BadParameter, AlreadyExists are client errors that won't be fixed by retrying.
	// AccessDenied is NOT included because it could indicate expired credentials,
	// which may be resolved by reconnecting with refreshed credentials from tbot.
	if trace.IsNotFound(err) || trace.IsBadParameter(err) || trace.IsAlreadyExists(err) {
		return true
	}

	return false
}
