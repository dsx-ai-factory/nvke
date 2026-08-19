// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"
)

// Target is the current tenant-cluster access material needed to run a manager.
// The Secret reference supports deletion lookup, while its UID distinguishes a
// deleted and recreated Secret from the prior object with the same name.
type Target struct {
	ClusterID           uuid.UUID
	ClusterName         string
	KubeconfigSecretRef types.NamespacedName
	KubeconfigSecretUID types.UID
	Kubeconfig          []byte
}

// Runtime is one running tenant controller manager.
type Runtime interface {
	Done() <-chan error
	Stop()
}

// RuntimeFactory starts a runtime and returns after its caches are ready.
type RuntimeFactory interface {
	Start(ctx context.Context, target Target) (Runtime, error)
}

// ManagerSetOptions bounds tenant-manager startup and retry behavior.
type ManagerSetOptions struct {
	MaxConcurrentStarts int
	StartupJitter       time.Duration
	InitialRetryBackoff time.Duration
	MaxRetryBackoff     time.Duration
}

// DefaultManagerSetOptions is sized for the initial target of 20 active tenant
// managers without allowing all of them to connect to their API servers at once.
func DefaultManagerSetOptions() ManagerSetOptions {
	return ManagerSetOptions{
		MaxConcurrentStarts: 2,
		StartupJitter:       5 * time.Second,
		InitialRetryBackoff: time.Second,
		MaxRetryBackoff:     time.Minute,
	}
}

// tenantManagerGeneration owns one kubeconfig generation for one cluster.
// Upsert replaces it when that cluster's Secret is recreated or its kubeconfig
// changes. One generation may start more than one Runtime while recovering from
// startup failures or unexpected exits.
type tenantManagerGeneration struct {
	// target is the copied Secret identity and kubeconfig used for every start
	// attempt made by this generation.
	target Target

	// cancel stops this generation's start/retry loop. If its manager is running,
	// cancellation also stops the manager and its registered controllers.
	cancel context.CancelFunc

	// stopped is closed after runGeneration has returned and the running manager,
	// if any, has completely stopped. A replacement generation waits for this
	// signal.
	stopped chan struct{}

	// waitFor is nil for an initial start. During kubeconfig rotation it points
	// to the previous generation's stopped channel, preventing two managers for
	// the same cluster from running at once.
	waitFor <-chan struct{}
}

// ManagerSet owns the lifecycle of all tenant controller managers in this CWE
// process. SecretReconciler calls Upsert, RemoveKubeconfigSecret, or
// RemoveCluster; ManagerSet itself does not watch Kubernetes. Each generation
// independently starts, retries, runs, and stops one tenant manager.
type ManagerSet struct {
	// logger records lifecycle changes for individual tenant managers.
	logger *slog.Logger

	// factory constructs the controller-runtime manager connected to a tenant
	// cluster.
	factory RuntimeFactory

	// options control startup concurrency, jitter, and retry backoff.
	options ManagerSetOptions

	// startSlots is a semaphore. It limits concurrent calls to factory.Start so
	// many tenant clusters becoming available together do not overwhelm CWE or
	// their API servers.
	startSlots chan struct{}

	// mu protects all fields below it.
	mu sync.Mutex
	// generations maps the stable CWE cluster UUID to that cluster's current
	// kubeconfig generation. A generation may be waiting, starting, retrying, or
	// running.
	generations map[uuid.UUID]*tenantManagerGeneration
	// kubeconfigSecretOwners is the reverse lookup used when a Secret deletion
	// event contains namespace/name but the deleted object can no longer be read.
	kubeconfigSecretOwners map[types.NamespacedName]uuid.UUID
	// closed rejects new Upserts after CWE shutdown begins.
	closed bool
	// wg tracks every runGeneration goroutine so shutdown waits for all managers
	// to finish stopping.
	wg sync.WaitGroup
	// shutdownOnce makes Shutdown idempotent; shutdownDone lets simultaneous
	// callers wait for the first Shutdown call to finish.
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

func NewManagerSet(ctx context.Context, logger *slog.Logger, factory RuntimeFactory, options ManagerSetOptions) (*ManagerSet, error) {
	if ctx == nil {
		return nil, errors.New("tenant manager set context is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if factory == nil {
		return nil, errors.New("tenant runtime factory is required")
	}
	if options.MaxConcurrentStarts <= 0 {
		return nil, errors.New("maximum concurrent tenant runtime starts must be greater than zero")
	}
	if options.StartupJitter < 0 {
		return nil, errors.New("tenant runtime startup jitter must not be negative")
	}
	if options.InitialRetryBackoff <= 0 {
		return nil, errors.New("initial tenant runtime retry backoff must be greater than zero")
	}
	if options.MaxRetryBackoff < options.InitialRetryBackoff {
		return nil, errors.New("maximum tenant runtime retry backoff must not be less than the initial backoff")
	}

	managerSet := &ManagerSet{
		logger:                 logger,
		factory:                factory,
		options:                options,
		startSlots:             make(chan struct{}, options.MaxConcurrentStarts),
		generations:            make(map[uuid.UUID]*tenantManagerGeneration),
		kubeconfigSecretOwners: make(map[types.NamespacedName]uuid.UUID),
		shutdownDone:           make(chan struct{}),
	}
	context.AfterFunc(ctx, managerSet.Shutdown)
	return managerSet, nil
}

// Upsert starts a manager for new access material or rotates only the affected
// manager when the Secret object or kubeconfig contents change.
func (s *ManagerSet) Upsert(ctx context.Context, target Target) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	target.Kubeconfig = append([]byte(nil), target.Kubeconfig...)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("tenant manager set is shut down")
	}
	if clusterID, exists := s.kubeconfigSecretOwners[target.KubeconfigSecretRef]; exists && clusterID != target.ClusterID {
		return fmt.Errorf("tenant kubeconfig Secret %q is already associated with cluster %s", target.KubeconfigSecretRef.String(), clusterID)
	}

	var waitFor <-chan struct{}
	if generation, exists := s.generations[target.ClusterID]; exists {
		if sameGenerationTarget(generation.target, target) {
			return nil
		}
		generation.cancel()
		delete(s.kubeconfigSecretOwners, generation.target.KubeconfigSecretRef)
		waitFor = generation.stopped
	}

	s.startGenerationLocked(ctx, target, waitFor)
	return nil
}

// RemoveCluster stops the manager for a cluster that is deleting, disabled, or
// no longer present in authoritative desired state.
func (s *ManagerSet) RemoveCluster(clusterID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeClusterLocked(clusterID)
}

// RemoveKubeconfigSecret stops the manager previously created from a deleted or
// invalid kubeconfig Secret. The Secret-reference index makes deletion
// independent of cluster name and preserves cluster UUID as runtime identity.
func (s *ManagerSet) RemoveKubeconfigSecret(secretRef types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clusterID, exists := s.kubeconfigSecretOwners[secretRef]
	if !exists {
		return
	}
	s.removeClusterLocked(clusterID)
}

func (s *ManagerSet) removeClusterLocked(clusterID uuid.UUID) {
	generation, exists := s.generations[clusterID]
	if !exists {
		return
	}
	generation.cancel()
	delete(s.generations, clusterID)
	delete(s.kubeconfigSecretOwners, generation.target.KubeconfigSecretRef)
	s.logger.Info("Stopping tenant controller manager", "clusterID", clusterID, "clusterName", generation.target.ClusterName)
}

func sameGenerationTarget(current, next Target) bool {
	return current.KubeconfigSecretRef == next.KubeconfigSecretRef &&
		current.KubeconfigSecretUID == next.KubeconfigSecretUID &&
		bytes.Equal(current.Kubeconfig, next.Kubeconfig)
}

func validateTarget(target Target) error {
	if target.ClusterID == uuid.Nil {
		return errors.New("tenant runtime target cluster ID is required")
	}
	if target.KubeconfigSecretRef.Namespace == "" || target.KubeconfigSecretRef.Name == "" {
		return fmt.Errorf("tenant runtime target %s has no kubeconfig Secret reference", target.ClusterID)
	}
	if target.KubeconfigSecretUID == "" {
		return fmt.Errorf("tenant runtime target %s has no kubeconfig Secret UID", target.ClusterID)
	}
	if len(target.Kubeconfig) == 0 {
		return fmt.Errorf("tenant runtime target %s has no kubeconfig", target.ClusterID)
	}
	return nil
}

func (s *ManagerSet) startGenerationLocked(ctx context.Context, target Target, waitFor <-chan struct{}) {
	// A reconcile context ends after one Secret event. Preserve its values for
	// manager construction, but let the manager set own the runtime lifetime.
	generationCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	generation := &tenantManagerGeneration{
		target:  target,
		cancel:  cancel,
		stopped: make(chan struct{}),
		waitFor: waitFor,
	}
	s.generations[target.ClusterID] = generation
	s.kubeconfigSecretOwners[target.KubeconfigSecretRef] = target.ClusterID
	s.wg.Add(1)
	go s.runGeneration(generationCtx, generation)
}

func (s *ManagerSet) runGeneration(ctx context.Context, generation *tenantManagerGeneration) {
	defer s.wg.Done()
	defer close(generation.stopped)

	if generation.waitFor != nil {
		select {
		case <-ctx.Done():
			return
		case <-generation.waitFor:
		}
	}

	attempt := 0
	for {
		delay := s.startDelay(generation.target.ClusterID, attempt)
		if !waitForDelay(ctx, delay) {
			return
		}
		if !s.acquireStartSlot(ctx) {
			return
		}

		runtime, err := s.factory.Start(ctx, generation.target)
		s.releaseStartSlot()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempt++
			s.logger.Warn("Tenant controller manager failed to start; will retry",
				"error", err,
				"clusterID", generation.target.ClusterID,
				"clusterName", generation.target.ClusterName,
				"attempt", attempt)
			continue
		}

		s.logger.Info("Tenant controller manager started",
			"clusterID", generation.target.ClusterID,
			"clusterName", generation.target.ClusterName)
		attempt = 0
		err = waitForRuntime(ctx, runtime)
		if ctx.Err() != nil {
			return
		}

		attempt++
		s.logger.Warn("Tenant controller manager stopped unexpectedly; will retry",
			"error", err,
			"clusterID", generation.target.ClusterID,
			"clusterName", generation.target.ClusterName,
			"attempt", attempt)
	}
}

func (s *ManagerSet) startDelay(clusterID uuid.UUID, attempt int) time.Duration {
	if attempt == 0 {
		if s.options.StartupJitter == 0 {
			return 0
		}
		seed := int64(binary.BigEndian.Uint32(clusterID[:4]))*1_000_000_007 +
			int64(binary.BigEndian.Uint32(clusterID[4:8]))
		return time.Duration(seed % (s.options.StartupJitter + 1).Nanoseconds())
	}

	delay := s.options.InitialRetryBackoff
	for range attempt - 1 {
		if delay >= s.options.MaxRetryBackoff/2 {
			return s.options.MaxRetryBackoff
		}
		delay *= 2
	}
	if delay > s.options.MaxRetryBackoff {
		return s.options.MaxRetryBackoff
	}
	return delay
}

func waitForDelay(ctx context.Context, delay time.Duration) bool {
	if delay == 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *ManagerSet) acquireStartSlot(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case s.startSlots <- struct{}{}:
		return true
	}
}

func (s *ManagerSet) releaseStartSlot() {
	<-s.startSlots
}

func waitForRuntime(ctx context.Context, runtime Runtime) error {
	select {
	case err, ok := <-runtime.Done():
		if !ok {
			return nil
		}
		return err
	case <-ctx.Done():
		runtime.Stop()
		<-runtime.Done()
		return ctx.Err()
	}
}

// Shutdown stops all runtimes and waits for their goroutines to finish.
func (s *ManagerSet) Shutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		for _, generation := range s.generations {
			generation.cancel()
		}
		s.generations = make(map[uuid.UUID]*tenantManagerGeneration)
		s.kubeconfigSecretOwners = make(map[types.NamespacedName]uuid.UUID)
		s.mu.Unlock()

		s.wg.Wait()
		close(s.shutdownDone)
	})
	<-s.shutdownDone
}
