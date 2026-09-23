// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

const (
	// coreAPIVersion is the version of the core (empty-group) API that every
	// resource in encryptableCoreV1GVR belongs to.
	coreAPIVersion                 = "v1"
	resourceSecrets                = "secrets"
	resourceConfigMaps             = "configmaps"
	resourcePersistentVolumeClaims = "persistentvolumeclaims"
	resourcePersistentVolumes      = "persistentvolumes"
	resourceServiceAccounts        = "serviceaccounts"
	resourceEvents                 = "events"
	resourceServices               = "services"
)

// encryptableCoreV1GVR maps encryption-config resource names (plural, lowercase)
// to core/v1 GroupVersionResource. This matches the default objectsToEncrypt list in the agent.
var encryptableCoreV1GVR = map[string]schema.GroupVersionResource{
	resourceSecrets:                {Group: "", Version: coreAPIVersion, Resource: resourceSecrets},
	resourceConfigMaps:             {Group: "", Version: coreAPIVersion, Resource: resourceConfigMaps},
	resourcePersistentVolumeClaims: {Group: "", Version: coreAPIVersion, Resource: resourcePersistentVolumeClaims},
	resourcePersistentVolumes:      {Group: "", Version: coreAPIVersion, Resource: resourcePersistentVolumes},
	resourceServiceAccounts:        {Group: "", Version: coreAPIVersion, Resource: resourceServiceAccounts},
	resourceEvents:                 {Group: "", Version: coreAPIVersion, Resource: resourceEvents},
	resourceServices:               {Group: "", Version: coreAPIVersion, Resource: resourceServices},
}

// clusterScopedEncryptResources lists plural resource names that are not namespaced.
var clusterScopedEncryptResources = map[string]struct{}{
	resourcePersistentVolumes: {},
}

// encryptRewriteBackoff limits conflict and transient rate-limit retries per object.
var encryptRewriteBackoff = wait.Backoff{
	Steps:    12,
	Duration: 500 * time.Millisecond,
	Factor:   1.5,
	Cap:      6 * time.Second,
	Jitter:   0.2,
}

const (
	encryptListPageSize   int64 = 500
	encryptRewriteWorkers       = 64
)

type encryptObjectRef struct {
	namespace string
	name      string
}

func formatEncryptObjectRef(plural string, ref encryptObjectRef) string {
	if ref.namespace == "" {
		return fmt.Sprintf("%s/%s", plural, ref.name)
	}
	return fmt.Sprintf("%s %s/%s", plural, ref.namespace, ref.name)
}

// RewriteEncryptableAPIObjects lists each resource kind (in order), then updates every object
// so the apiserver persists it with the active encryption configuration. Conflicts and transient
// HTTP 429 rate limits are retried per object using a fresh read + update.
//
// Per-object failures that are safe to ignore (object deleted between list and update)
// are logged and skipped so the rest of the rewrite continues.
// List failures for a kind still abort the run; context cancellation is always returned.
func RewriteEncryptableAPIObjects(ctx context.Context, resourcePlurals []string) error {
	dyn, err := getDynamicClient()
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	for _, plural := range resourcePlurals {
		if err := rewriteEncryptableResourceType(ctx, dyn, plural); err != nil {
			return err
		}
	}
	return nil
}

func rewriteEncryptableResourceType(ctx context.Context, dyn dynamic.Interface, plural string) error {
	gvr, ok := encryptableCoreV1GVR[plural]
	if !ok {
		return fmt.Errorf("unknown encryptable resource type %q", plural)
	}
	_, clusterScoped := clusterScopedEncryptResources[plural]

	refs, err := listEncryptableObjectRefs(ctx, dyn, gvr, plural, clusterScoped)
	if err != nil {
		return err
	}
	return rewriteEncryptableObjectRefs(ctx, dyn, gvr, plural, clusterScoped, refs)
}

// isSkippableEncryptRewriteError returns true for errors that should not abort the full rewrite:
// the object disappeared between the list and the update, a benign race because an object that
// no longer exists needs no re-encryption.
//
// Forbidden is deliberately not skippable. The rewrite exists to re-encrypt every object under
// the current key, so an object the agent cannot update is an object left under the old key.
// Skipping it would report a successful rewrite over data that is only partially re-encrypted,
// hiding the missing RBAC; failing loudly surfaces it instead.
func isSkippableEncryptRewriteError(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err) || apierrors.IsGone(err)
}

func listEncryptableObjectRefs(
	ctx context.Context,
	dyn dynamic.Interface,
	gvr schema.GroupVersionResource,
	plural string,
	clusterScoped bool,
) ([]encryptObjectRef, error) {
	ri := listResourceInterface(dyn, gvr, clusterScoped)
	var refs []encryptObjectRef
	listOpts := metav1.ListOptions{Limit: encryptListPageSize}
	for {
		list, err := ri.List(ctx, listOpts)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", plural, err)
		}
		for _, item := range list.Items {
			if clusterScoped {
				refs = append(refs, encryptObjectRef{name: item.GetName()})
				continue
			}
			ns := item.GetNamespace()
			nm := item.GetName()
			if ns == "" || nm == "" {
				slog.Warn("skipping list item missing namespace or name during encrypt rewrite",
					"resource", plural, "namespace", ns, "name", nm)
				continue
			}
			refs = append(refs, encryptObjectRef{namespace: ns, name: nm})
		}
		if list.GetContinue() == "" {
			break
		}
		listOpts.Continue = list.GetContinue()
	}
	return refs, nil
}

func listResourceInterface(dyn dynamic.Interface, gvr schema.GroupVersionResource, clusterScoped bool) dynamic.ResourceInterface {
	nri := dyn.Resource(gvr)
	if clusterScoped {
		return nri
	}
	// Cross-namespace list for namespaced resources (same as kubectl get -A).
	return nri.Namespace(metav1.NamespaceAll)
}

func objectResourceInterface(dyn dynamic.Interface, gvr schema.GroupVersionResource, clusterScoped bool, ref encryptObjectRef) dynamic.ResourceInterface {
	nri := dyn.Resource(gvr)
	if clusterScoped {
		return nri
	}
	return nri.Namespace(ref.namespace)
}

func rewriteEncryptableObjectRefs(
	ctx context.Context,
	dyn dynamic.Interface,
	gvr schema.GroupVersionResource,
	plural string,
	clusterScoped bool,
	refs []encryptObjectRef,
) error {
	if len(refs) == 0 {
		return nil
	}

	// workCtx is only for stopping the worker pool after the first object failure.
	// The original caller ctx still controls external cancellation and deadlines.
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	workerCount := encryptRewriteWorkerCount(len(refs))
	jobs := make(chan encryptObjectRef)
	errs := make(chan error, workerCount)

	worker := func() {
		for {
			select {
			case <-workCtx.Done():
				return
			case ref, ok := <-jobs:
				if !ok {
					return
				}
				if err := rewriteEncryptableObject(workCtx, dyn, gvr, plural, clusterScoped, ref); err != nil {
					contextDone := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
					if contextDone && context.Cause(workCtx) != nil {
						return
					}
					errs <- err
					cancel(err)
					return
				}
			}
		}
	}

	wg := startEncryptRewriteWorkers(workerCount, worker)
	sendEncryptRewriteJobs(workCtx, refs, jobs)
	wg.Wait()
	close(errs)

	return collectEncryptRewriteErrors(ctx, workCtx, errs)
}

func startEncryptRewriteWorkers(workerCount int, worker func()) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	return wg
}

func sendEncryptRewriteJobs(ctx context.Context, refs []encryptObjectRef, jobs chan<- encryptObjectRef) {
	defer close(jobs)
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return
		case jobs <- ref:
		}
	}
}

func encryptRewriteWorkerCount(refCount int) int {
	if refCount < encryptRewriteWorkers {
		return refCount
	}
	return encryptRewriteWorkers
}

func collectEncryptRewriteErrors(ctx, workCtx context.Context, errs <-chan error) error {
	var joinedErr error
	for err := range errs {
		joinedErr = errors.Join(joinedErr, err)
	}
	if joinedErr != nil {
		return joinedErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Cause(workCtx)
}

func rewriteEncryptableObject(
	ctx context.Context,
	dyn dynamic.Interface,
	gvr schema.GroupVersionResource,
	plural string,
	clusterScoped bool,
	ref encryptObjectRef,
) error {
	ri := objectResourceInterface(dyn, gvr, clusterScoped, ref)
	err := retryEncryptRewriteTooManyRequests(ctx, plural, ref, func(ctx context.Context) error {
		return retry.RetryOnConflict(encryptRewriteBackoff, func() error {
			cur, err := ri.Get(ctx, ref.name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			// Strip read-only fields the apiserver may reject or that grow unbounded.
			unstructured.RemoveNestedField(cur.Object, "metadata", "managedFields")
			_, err = ri.Update(ctx, cur, metav1.UpdateOptions{})
			return err
		})
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isSkippableEncryptRewriteError(err) {
		slog.Warn("skipping kubernetes object encrypt rewrite",
			"ref", formatEncryptObjectRef(plural, ref), "reason", err)
		return nil
	}
	return fmt.Errorf("%s: %w", formatEncryptObjectRef(plural, ref), err)
}

func retryEncryptRewriteTooManyRequests(
	ctx context.Context,
	plural string,
	ref encryptObjectRef,
	fn func(context.Context) error,
) error {
	var lastTooManyRequests error
	err := wait.ExponentialBackoffWithContext(ctx, encryptRewriteBackoff, func(ctx context.Context) (bool, error) {
		if err := fn(ctx); err != nil {
			if !apierrors.IsTooManyRequests(err) {
				return false, err
			}
			lastTooManyRequests = err
			slog.Debug("retrying kubernetes object encrypt rewrite after rate limit",
				"ref", formatEncryptObjectRef(plural, ref), "reason", err)
			return false, nil
		}
		return true, nil
	})
	if err != nil && lastTooManyRequests != nil && ctx.Err() == nil && wait.Interrupted(err) {
		return lastTooManyRequests
	}
	return err
}
