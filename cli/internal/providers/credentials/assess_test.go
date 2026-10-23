// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dsx-ai-factory/nvke/cli/internal/providers"
)

var now = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

var nicoIdentityKind = providers.IdentityKind{
	Group:    "infrastructure.cluster.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "nicoidentities",
	Kind:     "NicoIdentity",
}

var nicoIdentityGVR = schema.GroupVersionResource{
	Group:    "infrastructure.cluster.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "nicoidentities",
}

// unchecked returns an Identity in capnico-system, naming nico-credentials,
// created created ago and never checked.
func unchecked(name string, created time.Duration) Identity {
	return Identity{
		Namespace:  "capnico-system",
		Name:       name,
		Generation: 1,
		Created:    now.Add(-created),
		SecretName: "nico-credentials",
	}
}

// checkedAgo returns nico-default whose last check of its current spec
// finished age ago with the given Ready status and reason.
func checkedAgo(age time.Duration, status, reason string) Identity {
	id := unchecked("nico-default", 3*time.Hour)
	id.Ready = &Condition{Status: status, Reason: reason, Message: "message for " + reason, ObservedGeneration: 1}
	id.LastChecked = new(now.Add(-age))
	return id
}

// input returns an Input for a CAPNICo whose manager is running and watches
// everything, holding ids.
func input(ids ...Identity) Input {
	return Input{
		Kind:             nicoIdentityKind,
		CRDEstablished:   true,
		Identities:       ids,
		SecretNamespace:  "capnico-system",
		SecretName:       "nico-credentials",
		ManagerFound:     true,
		ManagerAvailable: true,
		Now:              now,
	}
}

func TestAssess(t *testing.T) {
	tests := []struct {
		name       string
		in         Input
		wantHealth Health
		wantCell   string
		wantDetail []string
		// wantExactDetail, when set, is the whole detail, so nothing may be
		// appended to it.
		wantExactDetail string
	}{
		{
			name: "a forbidden list names the missing permission",
			in: func() Input {
				in := input()
				in.ListErr = apierrors.NewForbidden(nicoIdentityGVR.GroupResource(), "", errors.New("denied"))
				return in
			}(),
			wantHealth: HealthUnreadable,
			wantDetail: []string{"forbidden", "list on nicoidentities.infrastructure.cluster.x-k8s.io in capnico-system"},
		},
		{
			name: "Identities naming other Secrets do not observe the provider's credentials",
			in: func() Input {
				other := checkedAgo(time.Minute, "True", "ValidationSucceeded")
				other.SecretName = "tenant-a-credentials"
				return input(other)
			}(),
			wantHealth: HealthNoIdentity,
		},
		{
			name:       "an Identity never checked in eleven minutes is not being observed",
			in:         input(unchecked("nico-default", 11*time.Minute)),
			wantHealth: HealthStale,
			wantDetail: []string{"not checked since it was created 11m ago"},
		},
		{
			name: "Ready published before its check time is pending",
			in: func() Input {
				id := unchecked("nico-default", time.Minute)
				id.Ready = &Condition{Status: "True", Reason: "ValidationSucceeded", ObservedGeneration: 1}
				return input(id)
			}(),
			wantHealth: HealthPending,
		},
		{
			name: "a spec change not yet checked is pending",
			in: func() Input {
				id := checkedAgo(2*time.Minute, "True", "ValidationSucceeded")
				id.Generation = 2
				return input(id)
			}(),
			wantHealth: HealthPending,
			wantDetail: []string{"generation 2 not checked yet"},
		},
		{
			name:       "exactly ten minutes is still current",
			in:         input(checkedAgo(StaleAfter, "True", "ValidationSucceeded")),
			wantHealth: HealthHealthy,
		},
		{
			name:       "a check just past ten minutes is stale and keeps the last result",
			in:         input(checkedAgo(10*time.Minute+time.Second, "True", "ValidationSucceeded")),
			wantHealth: HealthStale,
			wantCell:   "STALE",
			wantDetail: []string{"last checked 10m ago (Ready=True, ValidationSucceeded)"},
		},
		{
			name: "a check a second past the grace after its published next check time is stale and says how overdue",
			in: func() Input {
				id := checkedAgo(9*time.Minute+2*time.Second, "True", "ValidationSucceeded")
				id.NextCheck = new(id.LastChecked.Add(4*time.Minute + time.Second))
				return input(id)
			}(),
			wantHealth:      HealthStale,
			wantCell:        "STALE",
			wantExactDetail: "capnico-system/nico-default: last checked 9m ago, next check due 5m ago (Ready=True, ValidationSucceeded)",
		},
		{
			name:       "an unreadable Secret is unknown",
			in:         input(checkedAgo(time.Minute, "Unknown", "SecretReadFailed")),
			wantHealth: HealthUnknown,
			wantCell:   "UNKNOWN (SecretReadFailed)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Assess(tt.in)
			if got.Health != tt.wantHealth {
				t.Fatalf("Health = %s, want %s (detail %q)", got.Health, tt.wantHealth, got.Detail)
			}
			if tt.wantCell != "" && got.Cell() != tt.wantCell {
				t.Errorf("Cell() = %q, want %q", got.Cell(), tt.wantCell)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			if tt.wantExactDetail != "" && got.Detail != tt.wantExactDetail {
				t.Errorf("Detail = %q, want %q", got.Detail, tt.wantExactDetail)
			}
		})
	}
}
