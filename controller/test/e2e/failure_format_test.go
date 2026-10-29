// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"github.com/onsi/gomega/format"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func init() {
	format.RegisterCustomFormatter(formatError)
}

// forbiddenHint follows an error the API server refused on authorization.
const forbiddenHint = "(the identity the suite runs as is not allowed this; for the validation image, " +
	"see the RBAC it needs in controller/test/e2e/README.md)"

// formatError stands in for the fields Gomega prints after an error's
// message, which repeat the message once per wrapped layer and bury it. It
// prints nothing there but, for a request the API server refused on
// authorization, where to look.
func formatError(value any) (string, bool) {
	err, ok := value.(error)
	if !ok || err == nil {
		return "", false
	}
	if apierrors.IsForbidden(err) {
		return forbiddenHint, true
	}
	return "", true
}
