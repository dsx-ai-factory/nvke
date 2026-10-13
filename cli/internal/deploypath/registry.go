// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package deploypath

import (
	"fmt"
	"slices"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// Registries opens repositories over the network. Hosts listed in plainHTTP
// are used in the clear; nothing is probed. Credentials come from the Docker
// config, as helm and oras read them.
func Registries(plainHTTP []string) func(repository string) (oras.ReadOnlyTarget, error) {
	return func(repository string) (oras.ReadOnlyTarget, error) {
		repo, err := remote.NewRepository(repository)
		if err != nil {
			return nil, err
		}
		repo.PlainHTTP = slices.Contains(plainHTTP, repo.Reference.Registry)
		store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
		if err != nil {
			return nil, fmt.Errorf("docker credentials: %w", err)
		}
		repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: credentials.Credential(store)}
		return repo, nil
	}
}
