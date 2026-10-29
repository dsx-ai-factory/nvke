// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/onsi/gomega/format"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestUnit_FormatError(t *testing.T) {
	_, ok := formatError("not an error")
	require.False(t, ok)
	_, ok = formatError(error(nil))
	require.False(t, ok)

	wrapped := fmt.Errorf("listing nodes: %w", fmt.Errorf("through the relay: %w", errors.New("connection refused")))
	printed := format.Object(wrapped, 1)
	require.Equal(t, 1, strings.Count(printed, "connection refused"), "the message once, not each wrapper's fields: %q", printed)
	require.NotContains(t, printed, forbiddenHint)

	forbidden := fmt.Errorf("reading the tenant credential: %w",
		apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "nvke-validate-blue", errors.New("no RBAC")))
	printed = format.Object(forbidden, 1)
	require.Equal(t, 1, strings.Count(printed, "no RBAC"), "the message once: %q", printed)
	require.Contains(t, printed, forbiddenHint, "a refused request points at the RBAC")
}
