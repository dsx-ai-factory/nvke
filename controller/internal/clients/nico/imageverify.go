// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
)

// Image-verification modes for config.Config.ImageVerificationMode. The
// pre-flight runs in CreateMachine, after the OS profile resolves to a
// MachineImage and before any object is applied: a node image that cannot
// show a passing validation verdict should be refused before a NicoMachine
// exists, not discovered after a node boots from it.
const (
	// ImageVerificationOff skips the pre-flight entirely. The default.
	ImageVerificationOff = "off"
	// ImageVerificationWarn runs the pre-flight and logs a failure, but still
	// creates the machine. The rollout mode: it surfaces every image that
	// would be refused before enforce turns refusal on.
	ImageVerificationWarn = "warn"
	// ImageVerificationEnforce refuses to create the machine when the
	// pre-flight fails. Fail-closed: an unverifiable image is treated the
	// same as a failing one.
	ImageVerificationEnforce = "enforce"
)

// ErrImageVerificationFailed marks a machine creation refused by the image
// pre-flight. Whether the refusal is retryable is carried separately: a
// definitive rejection is wrapped shared.Terminal, an operational failure is
// not.
var (
	ErrImageVerificationFailed = errors.New("node image verification failed")

	// ErrVerdictRejected is the definitive policy "no". Implementations wrap
	// it when asking again cannot change the answer; any other error reads as
	// operational — still refused fail-closed, but retryable.
	ErrVerdictRejected = errors.New("image validation verdict rejected")
)

// ImageVerifier is the verdict-source seam, defined in shared so Activities
// can thread one through MachineCreateDeps (its injection path into this
// pre-flight) without importing a provider package. A nil verifier means no
// verdict source is configured, which warn mode reports and enforce mode
// refuses.
type ImageVerifier = shared.ImageVerifier

// verifyImage applies the configured image-verification mode. Behavior is
// pinned by imageverify_test.go; off must stay a true no-op so the default
// configuration is byte-for-byte today's behavior.
func verifyImage(ctx context.Context, mode string, verifier ImageVerifier, image *enginev1.MachineImage) error {
	switch mode {
	case "", ImageVerificationOff:
		return nil
	case ImageVerificationWarn, ImageVerificationEnforce:
	default:
		// An unrecognized mode is a configuration defect. Treating it as any
		// particular mode would silently widen or narrow enforcement, so it
		// is an error in every mode.
		return fmt.Errorf("unknown image verification mode %q", mode)
	}

	err := verdict(ctx, verifier, image)
	if err == nil {
		return nil
	}
	if mode == ImageVerificationWarn {
		slog.Warn("node image verification failed; proceeding (mode=warn)",
			"image", image.Name, "err", err)
		return nil
	}
	refusal := fmt.Errorf("%w: image %s: %w", ErrImageVerificationFailed, image.Name, err)
	// Permanent only when the answer cannot change: a definitive rejection,
	// or a nil verifier (static configuration). Outages stay retryable.
	if verifier == nil || errors.Is(err, ErrVerdictRejected) {
		return shared.Terminal("nico.ErrImageVerificationFailed", refusal)
	}
	return refusal
}

func verdict(ctx context.Context, verifier ImageVerifier, image *enginev1.MachineImage) error {
	if verifier == nil {
		return errors.New("no verdict source is configured")
	}
	return verifier.VerifyImage(ctx, image)
}
