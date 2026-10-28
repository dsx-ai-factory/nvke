// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shared

import (
	"context"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// ImageVerifier reports whether the resolved machine image may be booted.
// Which provenance it trusts, and how it checks it, belong to the
// implementation; this interface is the seam it plugs into. It lives here, not
// in the nico package, so Activities can hold and thread one through
// MachineCreateDeps without importing a provider package. Implementations wrap
// definitive rejections in the nico package's ErrImageRejected; anything else
// reads as a retryable operational failure.
type ImageVerifier interface {
	VerifyImage(ctx context.Context, image *enginev1.MachineImage) error
}
