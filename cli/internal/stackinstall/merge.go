// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// mergeValues layers values as Helm does: maps merge key by key, anything
// else replaces. The inputs are not modified.
func mergeValues(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, layer := range layers {
		for k, v := range layer {
			src, srcMap := v.(map[string]any)
			dst, dstMap := out[k].(map[string]any)
			switch {
			case srcMap && dstMap:
				out[k] = mergeValues(dst, src)
			case srcMap:
				out[k] = mergeValues(src)
			default:
				out[k] = v
			}
		}
	}
	return out
}

// mergedValues is what Helm sees for rel: its values files, then its overlay.
func mergedValues(rel Release) (map[string]any, error) {
	layers := make([]map[string]any, 0, len(rel.ValuesFiles)+1)
	for _, file := range rel.ValuesFiles {
		data, err := fs.ReadFile(stackFS, file)
		if err != nil {
			return nil, err
		}
		var values map[string]any
		if err := yaml.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		layers = append(layers, values)
	}
	return mergeValues(append(layers, rel.Overlay)...), nil
}

// checkPlaceholder refuses merged spire values that still carry the
// placeholder trust domain of values/spire.yaml.
func checkPlaceholder(values map[string]any) error {
	var hits []string
	if dig(values, "global", "spire", "trustDomain") == placeholderTrustDomain {
		hits = append(hits, "global.spire.trustDomain")
	}
	for i, id := range stringList(dig(values, "spire-server", "adminIDs")) {
		if strings.HasPrefix(id, "spiffe://"+placeholderTrustDomain+"/") {
			hits = append(hits, fmt.Sprintf("spire-server.adminIDs[%d]", i))
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Strings(hits)
	return fmt.Errorf("spire values still carry the placeholder trust domain %s at %s", placeholderTrustDomain, strings.Join(hits, ", "))
}

func dig(values map[string]any, keys ...string) any {
	var v any = values
	for _, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

func stringList(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, _ := item.(string)
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}
