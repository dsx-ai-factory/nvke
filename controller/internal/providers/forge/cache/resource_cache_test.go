// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package cache

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

func TestResourceCacheCreation(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Instance](time.Minute)

	require.NotNil(t, cache, "Expected non-nil cache")

	// Check that the cache has the expected ttl
	if cache.ttl != time.Minute {
		t.Errorf("Expected ttl %v, got %v", time.Minute, cache.ttl)
	}

	// Check that the cache is empty
	if len(cache.cache) != 0 {
		t.Errorf("Expected empty cache, got %d items", len(cache.cache))
	}
}

func TestHydrate(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Instance](time.Minute)

	// Create some test resources
	resources := []*forgesdk.Instance{
		{Id: forgesdk.PtrString("000")},
		{Id: forgesdk.PtrString("001")},
		{Id: forgesdk.PtrString("002")},
	}

	// Hydrate the cache with the test resources
	cache.Hydrate(resources)

	// Check that the cache has the expected number of items
	if cache.Count() != len(resources) {
		t.Errorf("Expected %d items in cache, got %d", len(resources), cache.Count())
	}

	// Create new test resources
	resources = []*forgesdk.Instance{
		{Id: forgesdk.PtrString("002")},
	}

	// Re-Hydrate the cache with the test resources
	// This will test that Hydrate flushed the cache
	cache.Hydrate(resources)

	// Check that the cache has the expected number of items
	if cache.Count() != len(resources) {
		t.Errorf("Expected %d items in cache, got %d", len(resources), cache.Count())
	}
}

func TestGetResource(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Subnet](time.Minute)

	// Create some test resources
	resources := []*forgesdk.Subnet{
		{Id: forgesdk.PtrString("000")},
		{Id: forgesdk.PtrString("001")},
		{Id: forgesdk.PtrString("002")},
	}

	// Hydrate the cache with the test resources
	cache.Hydrate(resources)

	// Test getting an existing resource
	resource, found := cache.Get("001")
	if !found {
		t.Errorf("Expected to find resource 001")
	}

	if resource.GetId() != "001" {
		t.Errorf("Expected resource ID 001, got %s", resource.GetId())
	}

	// Test getting a non-existing resource
	_, found = cache.Get("999")
	if found {
		t.Errorf("Expected not to find resource 999")
	}
}

func TestCount(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Machine](time.Minute)

	// Check that the initial count is zero
	if cache.Count() != 0 {
		t.Errorf("Expected initial count to be 0, got %d", cache.Count())
	}

	// Create some test resources
	resources := []*forgesdk.Machine{
		{Id: forgesdk.PtrString("000")},
		{Id: forgesdk.PtrString("001")},
		{Id: forgesdk.PtrString("002")},
	}

	// Hydrate the cache with the test resources
	cache.Hydrate(resources)

	// Check that the count matches the number of hydrated resources
	if cache.Count() != len(resources) {
		t.Errorf("Expected count to be %d, got %d", len(resources), cache.Count())
	}
}

func TestIsExpired(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.OperatingSystem](time.Minute)

	// Test that the cache is initially expired
	if !cache.IsExpired() {
		t.Errorf("Expected cache to be expired")
	}

	// Add a resource to the cache
	cache.Hydrate([]*forgesdk.OperatingSystem{
		{Id: forgesdk.PtrString("999")},
	})

	// Test that the cache is not expired
	if cache.IsExpired() {
		t.Errorf("Expected cache not to be expired")
	}
}

func TestHasNoExpiredResources(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.OperatingSystem](time.Minute)

	// Create some test resources
	resources := []*forgesdk.OperatingSystem{
		{Id: forgesdk.PtrString("000")},
		{Id: forgesdk.PtrString("001")},
		{Id: forgesdk.PtrString("002")},
	}

	// Hydrate the cache with the test resources
	cache.Hydrate(resources)

	// Test that the cache has no expired resources now
	if cache.IsExpired() {
		t.Errorf("Expected no expired resources, found some")
	}
}

func TestItems(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Instance](time.Minute)

	// Create some test resources
	resources := []*forgesdk.Instance{
		{Id: forgesdk.PtrString("000")},
		{Id: forgesdk.PtrString("001")},
		{Id: forgesdk.PtrString("002")},
	}

	// Hydrate the cache with the test resources
	cache.Hydrate(resources)

	// Test that the cache returns all items
	// note: don't use .Count(), we want to ensure we're getting the correct
	// number of actual items
	items := cache.Items()
	if len(items) != len(resources) {
		t.Errorf("Expected %d items, got %d", len(resources), len(items))
	}

	// now, make sure the items are not mutable
	const newID = "modified"
	for _, item := range cache.Items() {
		item.Id = forgesdk.PtrString(newID)
	}

	// Check that the original resources are unchanged
	for _, resource := range resources {
		if resource.GetId() == newID {
			t.Errorf("Expected resource %v to be unchanged", resource.GetId())
		}
	}
}

func TestGetTTL(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Instance](time.Minute)

	// Check that the initial TTL is correct
	if cache.GetTTL() != time.Minute {
		t.Errorf("Expected initial TTL to be %v, got %v", time.Minute, cache.GetTTL())
	}
}

func TestSetAndGetLastError(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Instance](time.Minute)

	// Set a last error
	cache.SetLastError(errors.New("test error"))

	// Check that the last error is correct
	if cache.GetLastError() == nil {
		t.Errorf("Expected last error to be set, got nil")
	}

	if cache.GetLastError().Error() != "test error" {
		t.Errorf("Expected last error to be 'test error', got %v", cache.GetLastError())
	}
}

func TestGetLastHydrationTime(t *testing.T) {
	// Create a new resource cache
	cache := NewResourceCache[forgesdk.Instance](time.Minute)

	// Check that the last hydration time is correct
	if !cache.GetLastHydrationTime().IsZero() {
		t.Errorf("Expected last hydration time to be zero, got %v", cache.GetLastHydrationTime())
	}

	// Hydrate the cache with some test data
	cache.Hydrate([]*forgesdk.Instance{
		{Id: forgesdk.PtrString("000")},
	})

	// Check that the last hydration time is updated
	if cache.GetLastHydrationTime().IsZero() {
		t.Errorf("Expected last hydration time to be set, got zero value")
	}
}
