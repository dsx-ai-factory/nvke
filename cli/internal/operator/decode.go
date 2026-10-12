// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	kjson "sigs.k8s.io/json"

	"github.com/dsx-ai-factory/nvke/cli/internal/util"
	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// Decode reads a portable request document for the kind from YAML or JSON and
// returns it as a Resource.
//
// The document is a single mapping with a metadata and a spec, and nothing
// else: apiVersion, kind, namespace and resourceVersion are supplied by nvkectl,
// so supplying them is an error. Unknown fields anywhere in the document are
// errors, as are a missing or invalid name and a spec of the wrong shape.
//
// Decode checks structure only. Required fields, value patterns and the other
// admission rules of the resource are enforced by the management cluster when
// the resource is created.
func Decode(kind Kind, data []byte) (Resource, error) {
	doc, err := util.YAMLToGenericMap(data)
	if errors.Is(err, util.ErrNotMapping) {
		return Resource{}, errors.New("a request document is one mapping with a metadata and a spec, not a list or a single value; create one resource per run")
	}
	if err != nil {
		return Resource{}, err
	}
	if err := rejectUnknownTopLevel(doc); err != nil {
		return Resource{}, err
	}

	metadata, err := decodeMetadata(doc["metadata"])
	if err != nil {
		return Resource{}, err
	}
	spec, err := decodeSpec(kind, doc["spec"])
	if err != nil {
		return Resource{}, err
	}
	return Resource{Metadata: metadata, Spec: spec}, nil
}

// Manifest returns r as a complete manifest for the kind, with the apiVersion
// and kind a management cluster needs. It adds nothing else.
func Manifest(kind Kind, r Resource) map[string]any {
	metadata := map[string]any{"name": r.Metadata.Name}
	if len(r.Metadata.Annotations) > 0 {
		annotations := make(map[string]any, len(r.Metadata.Annotations))
		for k, v := range r.Metadata.Annotations {
			annotations[k] = v
		}
		metadata["annotations"] = annotations
	}
	return map[string]any{
		"apiVersion": enginev1.SchemeGroupVersion.String(),
		"kind":       string(kind),
		"metadata":   metadata,
		"spec":       r.Spec,
	}
}

// rejectUnknownTopLevel fails on any field other than metadata and spec.
func rejectUnknownTopLevel(doc map[string]any) error {
	var unknown []string
	for key := range doc {
		if key != "metadata" && key != "spec" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf(
		"unknown field %q: a request document has only metadata and spec; nvkectl supplies apiVersion, kind and the rest",
		unknown[0],
	)
}

// decodeMetadata strictly decodes the metadata of a request.
func decodeMetadata(raw any) (Metadata, error) {
	if raw == nil {
		return Metadata{}, fmt.Errorf("metadata is required")
	}
	var m Metadata
	if err := decodeStrict(raw, &m); err != nil {
		return Metadata{}, fmt.Errorf("invalid metadata: %w", err)
	}
	if m.Name == "" {
		return Metadata{}, fmt.Errorf("metadata.name is required")
	}
	if problems := validation.IsDNS1123Subdomain(m.Name); len(problems) > 0 {
		return Metadata{}, fmt.Errorf("metadata.name %q is not a valid name: %s", m.Name, strings.Join(problems, "; "))
	}
	return m, nil
}

// decodeSpec strictly decodes the spec of a request against the kind's typed
// spec, and returns it as the generic document to send. The typed value is
// used only to reject unknown fields and wrongly typed values: sending it
// instead would add the zero value of every field the operator left out.
func decodeSpec(kind Kind, raw any) (map[string]any, error) {
	if raw == nil {
		return nil, fmt.Errorf("spec is required")
	}
	typed, err := newSpec(kind)
	if err != nil {
		return nil, err
	}
	if err := decodeStrict(raw, typed); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	// The document goes through JSON so every value is a type the dynamic
	// client can carry, whatever numeric types the YAML decoder produced.
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encoding spec: %w", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("spec must be a mapping: %w", err)
	}
	return spec, nil
}

// newSpec returns a pointer to the zero typed spec of the kind.
func newSpec(kind Kind) (any, error) {
	switch kind {
	case KindMachineImage:
		return &enginev1.MachineImageSpec{}, nil
	case KindClusterRelease:
		return &enginev1.ClusterReleaseSpec{}, nil
	case KindNodePoolRelease:
		return &enginev1.NodePoolReleaseSpec{}, nil
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
}

// decodeStrict decodes the generic value raw into out. It rejects unknown and
// duplicate fields, and matches field names exactly: unlike encoding/json, a
// key with the wrong capitalization is an unknown field, not a match.
func decodeStrict(raw, out any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encoding: %w", err)
	}
	strictErrs, err := kjson.UnmarshalStrict(b, out)
	if err != nil {
		return err
	}
	return errors.Join(strictErrs...)
}
