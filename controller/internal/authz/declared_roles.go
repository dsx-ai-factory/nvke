// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"errors"
	"path/filepath"

	"github.com/cedar-policy/cedar-go/types"
)

// DeclaredRoleNames reads dir's entity files and returns the NKE::Role ids
// they declare -- for internal/auth.Config.Validate to check a RoleMapping's
// targets against, before internal/authz.Setup builds anything
// (design-authz-claim-based-roles.md, §7).
//
// internal/auth cannot import internal/authz to get this itself:
// middleware.go already imports internal/auth, so the reverse import would
// cycle. cmd/worker/main.go calls this first and threads the result into
// auth.Options.KnownRoles, independent of and prior to authz.Setup.
//
// Implemented with the same loadEntities helper NewLocal itself uses
// (local.go), so this can never see a different set of declared roles than
// NewLocal would load from the identical directory.
func DeclaredRoleNames(dir string) (map[string]struct{}, error) {
	if dir == "" {
		return nil, errors.New("authz bundle directory is empty")
	}

	entities := types.EntityMap{}
	declaredIn := map[types.EntityUID]string{}
	for _, name := range entityFiles {
		if _, err := loadEntities(filepath.Join(dir, name), entities, declaredIn); err != nil {
			return nil, err
		}
	}

	names := make(map[string]struct{})
	for uid := range entities {
		if string(uid.Type) == RoleType {
			names[string(uid.ID)] = struct{}{}
		}
	}
	return names, nil
}
