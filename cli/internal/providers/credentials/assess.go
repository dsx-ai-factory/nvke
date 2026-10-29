// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/cli/internal/providers"
	"github.com/dsx-ai-factory/nvke/cli/internal/util"
)

// watchFilterLabel is the label a Cluster API manager started with
// --watch-filter=<value> requires on the objects it reconciles.
const watchFilterLabel = "cluster.x-k8s.io/watch-filter"

// Input is what Assess needs to know about one provider.
type Input struct {
	// Kind is the provider's Identity kind.
	Kind providers.IdentityKind

	// CRDEstablished reports whether the CRD serving Kind is established and
	// serving a version.
	CRDEstablished bool

	// Identities are the Identities of Kind listed in SecretNamespace, and
	// ListErr the error listing them returned, if any.
	Identities []Identity
	ListErr    error

	// SecretNamespace and SecretName locate the credentials Secret the
	// provider's manager uses.
	SecretNamespace string
	SecretName      string

	// ManagerFound and ManagerAvailable describe the provider's manager
	// Deployment.
	ManagerFound     bool
	ManagerAvailable bool

	// WatchNamespace and WatchFilter are the values of the manager's
	// watch-scope flags, empty when the manager watches everything.
	WatchNamespace string
	WatchFilter    string

	// Now is the time freshness is judged against.
	Now time.Time
}

// Assess classifies a provider's credential health from the Identities that
// name its credentials Secret. It performs no I/O.
//
// When several Identities name the Secret, the one with a current, fresh
// result is reported, then the most recently checked, then the first by name;
// the others are listed in AdditionalIdentities.
func Assess(in Input) *Credentials {
	c := &Credentials{}

	switch {
	case !in.CRDEstablished:
		c.Health = HealthUnsupported
		c.Detail = fmt.Sprintf("the provider does not serve %s (no established CRD %s)", in.Kind.Kind, in.Kind.CRDName())
		return c
	case apierrors.IsNotFound(in.ListErr):
		// The CRD went away after it was found established, which is the same
		// answer as never having it.
		c.Health = HealthUnsupported
		c.Detail = fmt.Sprintf("the provider does not serve %s (%s not found)", in.Kind.Kind, in.Kind.CRDName())
		return c
	case apierrors.IsForbidden(in.ListErr):
		c.Health = HealthUnreadable
		c.Detail = fmt.Sprintf("listing %s in %s is forbidden: the site kubeconfig needs list on %s in %s",
			in.Kind.Resource, in.SecretNamespace, in.Kind.CRDName(), in.SecretNamespace)
		return c
	case in.ListErr != nil:
		c.Health = HealthUnreadable
		c.Detail = fmt.Sprintf("listing %s in %s: %v", in.Kind.Resource, in.SecretNamespace, in.ListErr)
		return c
	}

	var candidates []Identity
	for _, id := range in.Identities {
		if !id.Deleting && id.SecretName == in.SecretName {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		c.Health = HealthNoIdentity
		c.Detail = fmt.Sprintf("no %s in %s names Secret %s; create one to observe credential health",
			in.Kind.Kind, in.SecretNamespace, in.SecretName)
		return c
	}

	slices.SortStableFunc(candidates, func(a, b Identity) int {
		aGood := isCurrent(a) && !isStale(a, in.Now)
		bGood := isCurrent(b) && !isStale(b, in.Now)
		switch {
		case aGood != bGood:
			if aGood {
				return -1
			}
			return 1
		}
		// A missing check time sorts as the zero time, after every real check.
		aChecked, bChecked := cmp.Or(a.LastChecked, new(time.Time)), cmp.Or(b.LastChecked, new(time.Time))
		return cmp.Or(bChecked.Compare(*aChecked), cmp.Compare(a.Name, b.Name))
	})
	pick := candidates[0]
	for _, other := range candidates[1:] {
		c.AdditionalIdentities = append(c.AdditionalIdentities, ObjectRef{Namespace: other.Namespace, Name: other.Name})
	}
	c.Identity = &IdentityRef{
		APIVersion: in.Kind.APIVersion(),
		Kind:       in.Kind.Kind,
		Namespace:  pick.Namespace,
		Name:       pick.Name,
		Generation: pick.Generation,
	}
	c.Ready = pick.Ready
	if pick.LastChecked != nil {
		c.LastCheckedTime = new(metav1.NewTime(*pick.LastChecked))
	}
	if pick.NextCheck != nil {
		c.NextCheckTime = new(metav1.NewTime(*pick.NextCheck))
	}

	ref := pick.Namespace + "/" + pick.Name
	switch {
	case pick.LastCheckedInvalid:
		c.Health = HealthUnknown
		c.Detail = ref + ": lastCheckedTime is not a valid time"
	case pick.LastChecked != nil && pick.LastChecked.After(in.Now.Add(futureTolerance)):
		c.Health = HealthUnknown
		c.Detail = ref + ": lastCheckedTime is in the future; check the clock on this machine"
	case isStale(pick, in.Now):
		c.Health = HealthStale
		// Name a reason the manager has not checked the Identity only when
		// nvkectl can see one.
		var cause string
		switch {
		case !in.ManagerFound:
			cause = "the provider manager was not found"
		case in.WatchNamespace != "" && in.WatchNamespace != pick.Namespace:
			cause = "the manager only watches namespace " + in.WatchNamespace
		case in.WatchFilter != "" && pick.Labels[watchFilterLabel] != in.WatchFilter:
			cause = fmt.Sprintf("the Identity lacks label %s=%s", watchFilterLabel, in.WatchFilter)
		case !in.ManagerAvailable:
			cause = "the provider manager is not available"
		}
		if cause != "" {
			cause = "; " + cause
		}
		if pick.LastChecked == nil {
			c.Detail = fmt.Sprintf("%s: not checked since it was created %s ago%s",
				ref, util.FormatDuration(in.Now.Sub(pick.Created)), cause)
		} else {
			last := "no Ready condition"
			if pick.Ready != nil {
				last = "Ready=" + pick.Ready.Status
				if pick.Ready.Reason != "" {
					last += ", " + pick.Ready.Reason
				}
			}
			due := ""
			if next := nextCheckDue(pick); next != nil {
				due = ", next check due " + util.FormatDuration(in.Now.Sub(*next)) + " ago"
			}
			c.Detail = fmt.Sprintf("%s: last checked %s ago%s (%s)%s",
				ref, util.FormatDuration(in.Now.Sub(*pick.LastChecked)), due, last, cause)
		}
	case !isCurrent(pick):
		c.Health = HealthPending
		c.Detail = ref + ": not checked yet"
		if pick.Ready != nil && pick.LastChecked != nil && pick.Ready.ObservedGeneration != pick.Generation {
			c.Detail = fmt.Sprintf("%s: generation %d not checked yet", ref, pick.Generation)
		}
	default:
		switch metav1.ConditionStatus(pick.Ready.Status) {
		case metav1.ConditionTrue:
			c.Health = HealthHealthy
		case metav1.ConditionFalse:
			c.Health = HealthFailing
		default:
			c.Health = HealthUnknown
		}
		c.Detail = ref
		if pick.Ready.Message != "" {
			c.Detail = ref + ": " + pick.Ready.Message
		}
	}
	return c
}

// isCurrent reports whether id carries a completed check of its current spec.
func isCurrent(id Identity) bool {
	return id.Ready != nil && id.LastChecked != nil && id.Ready.ObservedGeneration == id.Generation
}

// isStale reports whether id's result is overdue at now. When the provider
// published a usable next check time, the result is overdue once now is more
// than nextCheckGrace past it. Otherwise it is overdue once its newest evidence
// is older than StaleAfter. The evidence is the last check, or, for an Identity
// never checked, its creation: an Identity that has waited longer than
// StaleAfter for its first check is not being observed.
func isStale(id Identity, now time.Time) bool {
	if next := nextCheckDue(id); next != nil {
		return now.Sub(*next) > nextCheckGrace
	}
	evidence := id.Created
	if id.LastChecked != nil {
		evidence = *id.LastChecked
	}
	return now.Sub(evidence) > StaleAfter
}

// nextCheckDue returns the next check time id's provider published, or nil
// when it published none that describes the reported check. A next check time
// is usable only when it is later than the last check, and no more than
// StaleAfter after it.
func nextCheckDue(id Identity) *time.Time {
	if id.NextCheck == nil || id.LastChecked == nil {
		return nil
	}
	// A provider that does not write the field advances lastCheckedTime and
	// leaves an older value in place.
	if !id.NextCheck.After(*id.LastChecked) {
		return nil
	}
	// Bound how long a published value can keep a result current, so a
	// far-future time cannot hide a stopped manager past the age fallback.
	if id.NextCheck.After(id.LastChecked.Add(StaleAfter)) {
		return nil
	}
	return id.NextCheck
}
