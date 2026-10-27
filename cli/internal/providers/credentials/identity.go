// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyConditionType is the condition an Identity reports its check result on.
const readyConditionType = "Ready"

// Identity is the part of one Identity object that classification reads.
type Identity struct {
	Namespace  string
	Name       string
	Generation int64
	Created    time.Time
	Deleting   bool
	Labels     map[string]string

	// SecretName is spec.credentialsRef.name: the Secret, in the Identity's
	// own namespace, whose credentials the provider checks.
	SecretName string

	// Ready is the Ready condition, or nil when the provider has published
	// none.
	Ready *Condition

	// LastChecked is status.lastCheckedTime, or nil when absent.
	// LastCheckedInvalid is set instead when the field is present but is not a
	// valid time.
	LastChecked        *time.Time
	LastCheckedInvalid bool

	// NextCheck is status.nextCheckTime, when the provider next checks the
	// Identity on its own schedule, or nil when absent or not a valid time.
	NextCheck *time.Time
}

// decode extracts an Identity from u. It never fails: a field that is missing
// or of an unexpected type is left at its zero value, so one malformed object
// cannot hide the others, and fields a newer provider adds are ignored.
func decode(u *unstructured.Unstructured) Identity {
	id := Identity{
		Namespace:  u.GetNamespace(),
		Name:       u.GetName(),
		Generation: u.GetGeneration(),
		Created:    u.GetCreationTimestamp().Time,
		Deleting:   u.GetDeletionTimestamp() != nil,
		Labels:     u.GetLabels(),
	}
	id.SecretName, _, _ = unstructured.NestedString(u.Object, "spec", "credentialsRef", "name")

	if raw, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "lastCheckedTime"); found && raw != nil {
		if t, ok := parseTime(raw); ok {
			id.LastChecked = &t
		} else {
			id.LastCheckedInvalid = true
		}
	}

	// A next check time that is not a valid time is treated as absent: the
	// provider has published no usable schedule.
	raw, _, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "nextCheckTime")
	if t, ok := parseTime(raw); ok {
		id.NextCheck = &t
	}

	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != readyConditionType {
			continue
		}
		id.Ready = decodeCondition(m)
		break
	}
	return id
}

// decodeCondition copies the fields of one condition that a reader reports.
func decodeCondition(m map[string]any) *Condition {
	c := &Condition{}
	c.Status, _ = m["status"].(string)
	c.Reason, _ = m["reason"].(string)
	c.Message, _ = m["message"].(string)

	// Objects decoded from the API server carry integers as int64; objects
	// built from JSON elsewhere may carry them as float64.
	switch g := m["observedGeneration"].(type) {
	case int64:
		c.ObservedGeneration = g
	case float64:
		c.ObservedGeneration = int64(g)
	}

	if t, ok := parseTime(m["lastTransitionTime"]); ok {
		c.LastTransitionTime = new(metav1.NewTime(t))
	}
	return c
}

// parseTime parses an RFC 3339 timestamp held in an unstructured field.
func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
