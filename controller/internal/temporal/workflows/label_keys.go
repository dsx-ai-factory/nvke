// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// A workflow cannot read live configuration: a replay must take the same
// branches the original run did. Each workflow that touches a domain-scoped
// key therefore carries the label domain in its input, captured when the
// workflow starts, and builds its keys from that. An input recorded before the
// domain was configurable has no domain, and keys.New reads that as the legacy
// domain, which is what those workflows wrote.

// labelRemovals returns one "key-" removal argument for every form of each key,
// so a key written under the legacy domain is removed too.
func labelRemovals(ks ...keys.Key) []string {
	var out []string
	for _, k := range ks {
		for _, name := range k.All() {
			out = append(out, name+"-")
		}
	}
	return out
}
