// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func dbCloudInitFilesFromAPI(files []api.CloudInitFile) []db.CloudInitFile {
	if len(files) == 0 {
		return nil
	}
	dbFiles := make([]db.CloudInitFile, len(files))
	for i, file := range files {
		owner := ""
		if file.Owner != nil {
			owner = *file.Owner
		}
		permissions := ""
		if file.Permissions != nil {
			permissions = *file.Permissions
		}
		dbFiles[i] = db.CloudInitFile{
			Path:        file.Path,
			Owner:       owner,
			Permissions: permissions,
			Content:     file.Content,
		}
	}
	return dbFiles
}

func apiCloudInitFilesFromDB(files []db.CloudInitFile) *[]api.CloudInitFile {
	if len(files) == 0 {
		return nil
	}
	apiFiles := make([]api.CloudInitFile, len(files))
	for i, file := range files {
		var owner *string
		if file.Owner != "" {
			owner = &file.Owner
		}
		var permissions *string
		if file.Permissions != "" {
			permissions = &file.Permissions
		}
		apiFiles[i] = api.CloudInitFile{
			Path:        file.Path,
			Owner:       owner,
			Permissions: permissions,
			Content:     file.Content,
		}
	}
	return &apiFiles
}
