// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile

import (
	"context"

	"github.com/google/uuid"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// OwnerLabelPrefix is the label-key prefix used to record DB-row ownership.
	OwnerLabelPrefix = "owner.nke.nvidia.com"
)

// OwnerLabel returns the label key that records an owner of the given kind.
func OwnerLabel(kind Kind) string {
	return OwnerLabelPrefix + "/" + string(kind)
}

// SetOwner records that object is owned by the DB row identified by kind and
// id. One owner can be recorded per kind.
func SetOwner(object client.Object, kind Kind, id uuid.UUID) {
	objectLabels := object.GetLabels()
	if objectLabels == nil {
		objectLabels = make(map[string]string)
	}
	objectLabels[OwnerLabel(kind)] = id.String()
	object.SetLabels(objectLabels)
}

// SelectorForKind returns a selector matching objects with an owner label for
// kind.
func SelectorForKind(kind Kind) *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      OwnerLabel(kind),
			Operator: metav1.LabelSelectorOpExists,
		}},
	}
}

// RequestForOwner maps an object to the DB reconcile key stored in its owner
// label. Missing or malformed labels produce no request.
func RequestForOwner(kind Kind) func(context.Context, client.Object) []Key {
	return func(ctx context.Context, object client.Object) []Key {
		raw, ok := object.GetLabels()[OwnerLabel(kind)]
		if !ok {
			return nil
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			crlog.FromContext(ctx).Error(err, "Owner label value is not a valid UUID", "kind", kind, "value", raw)
			return nil
		}
		return []Key{{Kind: kind, ID: id}}
	}
}

// Owns returns a Kubernetes watch for objects labeled as owned by a DB row
// of this kind. The owner label is "owner.nke.nvidia.com/<kind>"
// and its value must be the row's UUID.
func (k Kind) Owns(object client.Object, namespace string) KubeWatch {
	return KubeWatch{
		Object:        object,
		Namespace:     namespace,
		LabelSelector: SelectorForKind(k),
		Map:           RequestForOwner(k),
	}
}
