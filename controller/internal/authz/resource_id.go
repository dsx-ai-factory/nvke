// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"fmt"

	"github.com/google/uuid"
)

// canonicalID returns the identifier in the one spelling the handler acts on.
//
// The handler receives a parsed value, not the bytes from the path, and a UUID
// has four spellings that parse to the same value: canonical, brace-wrapped,
// bare 32 hex digits, and urn:uuid-prefixed. Naming the object by whichever one
// the caller typed makes four Cedar entities out of one cluster, of which a
// resource-scoped forbid names one — so the other three walk past it.
func canonicalID(op Operation, value string) (string, error) {
	switch op.IDFormat {
	case FormatUUID:
		parsed, err := uuid.Parse(value)
		if err != nil {
			// The binding will refuse this too, with a 400. Denying first is
			// this middleware's position: a caller who may not call an endpoint
			// learns that before it learns whether its input was well formed.
			return "", fmt.Errorf("%w: %s is not a %s: %w", ErrInvalidQuery, op.IDParam, FormatUUID, err)
		}
		return parsed.String(), nil
	default:
		// The generator refuses to emit a format this does not handle, so
		// reaching here means the table was written by something else.
		return "", fmt.Errorf("%w: %s has format %q, which cannot be canonicalized",
			ErrInvalidQuery, op.IDParam, op.IDFormat)
	}
}

const (
	// FormatUUID is the OpenAPI format of every path parameter in this API, and
	// the only one canonicalID knows. hack/gen-authz-actions refuses to generate
	// a table naming another one.
	FormatUUID = "uuid"
)
