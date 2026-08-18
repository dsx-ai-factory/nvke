// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"sync"
	"time"
)

// Resource is an interface for resources that can be cached.
type Resource[T any] interface {
	*T

	GetId() string
}

// ResourceCache is a cache for resources with a limited lifetime.
type ResourceCache[T any, R Resource[T]] struct {
	// ttl is the time-to-live for cached resources
	ttl time.Duration

	// cache holds the current state of all cached resources; the key is the id of the resource
	cache map[string]R

	// mu is a mutex to protect access to the cache
	mu sync.RWMutex

	// lastError enables consumers of this cache to track the last error that occurred while
	// working with the cache
	lastError error

	// lastHydrationTime tracks the last time the cache was hydrated
	lastHydrationTime time.Time
}

// NewResourceCache creates a new resource cache.
func NewResourceCache[T any, R Resource[T]](ttl time.Duration) *ResourceCache[T, R] {
	return &ResourceCache[T, R]{
		ttl:   ttl,
		cache: make(map[string]R),
	}
}

// Hydrate completely replaces the contents of the cache with the new items
func (rc *ResourceCache[T, R]) Hydrate(items []R) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.cache = make(map[string]R)

	for _, item := range items {
		rc.cache[item.GetId()] = item
	}

	rc.lastHydrationTime = time.Now()
	rc.lastError = nil
}

// Get retrieves a resource from the cache.
// It returns the resource or a zero value resource and a boolean indicating
// whether the resource was found.
func (rc *ResourceCache[T, R]) Get(id string) (R, bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	item, exists := rc.cache[id]
	if !exists {
		var zero R
		return zero, false
	}

	copiedItem := *item
	return &copiedItem, true
}

// Count returns the number of resources in the cache.
func (rc *ResourceCache[T, R]) Count() int {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return len(rc.cache)
}

// HasExpiredResource checks if any resource in the cache has expired.
func (rc *ResourceCache[T, R]) IsExpired() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	expirationTime := rc.lastHydrationTime.Add(rc.ttl)

	return time.Now().After(expirationTime)
}

// Items returns a copy of all items in the cache.
func (rc *ResourceCache[T, R]) Items() []R {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	items := make([]R, 0, len(rc.cache))
	for _, resourceItem := range rc.cache {
		itemCopy := *resourceItem
		items = append(items, &itemCopy)
	}

	return items
}

// GetTTL returns the time-to-live for cached resources.
func (rc *ResourceCache[T, R]) GetTTL() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.ttl
}

// SetLastError sets the last error that occurred while working with the cache.
func (rc *ResourceCache[T, R]) SetLastError(err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.lastError = err
}

// GetLastError returns the last error that occurred while working with the cache.
func (rc *ResourceCache[T, R]) GetLastError() error {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.lastError
}

// GetLastHydrationTime returns the last time the cache was hydrated.
func (rc *ResourceCache[T, R]) GetLastHydrationTime() time.Time {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.lastHydrationTime
}
