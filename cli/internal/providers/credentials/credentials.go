// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package credentials reports the credential health an infrastructure
// provider's manager publishes about itself.
//
// A provider that supports it checks its own credentials on a timer and
// records the outcome on an Identity resource. This package reads those
// Identities and classifies the one that observes the provider's credentials
// Secret. It never triggers a check and never reads the Secret, so what it
// reports is the provider's last completed check and how long ago it ran.
package credentials

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Health is a one-word summary of a provider's credential health.
type Health string

const (
	// HealthHealthy means the provider's last check authenticated and passed
	// its baseline, recently enough to be current. It does not prove that
	// provisioning will succeed.
	HealthHealthy Health = "HEALTHY"

	// HealthFailing means the last check found the credentials missing,
	// invalid, rejected, or forbidden.
	HealthFailing Health = "FAILING"

	// HealthUnknown means the last check could not decide, for example during
	// a backend outage, or that its check time cannot be trusted.
	HealthUnknown Health = "UNKNOWN"

	// HealthStale means no check has completed within StaleAfter when one
	// should have.
	HealthStale Health = "STALE"

	// HealthPending means the Identity, or its current spec, has not been
	// checked yet.
	HealthPending Health = "PENDING"

	// HealthNoIdentity means no Identity names the provider's credentials
	// Secret, so nothing observes it.
	HealthNoIdentity Health = "NO-IDENTITY"

	// HealthUnsupported means the provider installed on the site does not
	// serve its Identity kind.
	HealthUnsupported Health = "UNSUPPORTED"

	// HealthUnreadable means the Identities could not be listed.
	HealthUnreadable Health = "UNREADABLE"
)

// StaleAfter is how old a provider's last completed check may be before its
// result is no longer reported as current. Providers recheck about every five
// minutes, so this allows one slow or missed check.
const StaleAfter = 10 * time.Minute

// futureTolerance is how far ahead of this machine's clock a check time may be
// before it is treated as untrustworthy rather than current.
const futureTolerance = time.Minute

// Credentials is the credential health reported for one provider.
type Credentials struct {
	// Health summarises the provider's credential health.
	Health Health `json:"health"`

	// Detail explains Health for a person. Its wording is not stable.
	Detail string `json:"detail"`

	// StaleAfterSeconds is the age, in seconds, after which a check result is
	// reported as STALE.
	StaleAfterSeconds int64 `json:"staleAfterSeconds"`

	// Identity is the Identity the result was read from. Nil when none was
	// found.
	Identity *IdentityRef `json:"identity,omitempty"`

	// Ready is the Identity's Ready condition as the provider published it.
	// Nil when the provider has published none.
	Ready *Condition `json:"ready,omitempty"`

	// LastCheckedTime is when the provider's last reported check completed.
	LastCheckedTime *metav1.Time `json:"lastCheckedTime,omitempty"`

	// AdditionalIdentities lists other Identities that name the same Secret.
	// They observe the same credentials; Identity is the one reported.
	AdditionalIdentities []ObjectRef `json:"additionalIdentities,omitempty"`
}

// IdentityRef identifies the Identity a result was read from, and the spec
// generation it had when read.
type IdentityRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
}

// ObjectRef names a namespaced object.
type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Condition is a provider-published condition, copied as found.
type Condition struct {
	Status             string       `json:"status"`
	Reason             string       `json:"reason,omitempty"`
	Message            string       `json:"message,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// Cell returns the CREDENTIALS table cell for c: "-" when c is nil, otherwise
// the health. The provider's reason follows in parentheses when it explains the
// health: always for FAILING, and for UNKNOWN when the provider itself reported
// an inconclusive check.
func (c *Credentials) Cell() string {
	if c == nil {
		return "-"
	}
	if c.Ready == nil || c.Ready.Reason == "" {
		return string(c.Health)
	}
	switch {
	case c.Health == HealthFailing,
		c.Health == HealthUnknown && c.Ready.Status != string(metav1.ConditionTrue) && c.Ready.Status != string(metav1.ConditionFalse):
		return string(c.Health) + " (" + c.Ready.Reason + ")"
	default:
		return string(c.Health)
	}
}
