// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli

import (
	"encoding/json"
	"fmt"

	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
)

// APIVersion versions every JSON result. A breaking change to any result's
// shape moves it.
const APIVersion = "os.nvke.dev/v1alpha1"

// envelope wraps every JSON result so a consumer can check what it is reading.
type envelope struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Data       any    `json:"data"`
}

// writeResult writes one result to stdout, as text or as a JSON envelope
// according to the global --output flag.
func writeResult(c *cli.Context, kind string, data any, text string) error {
	if c.String("output") != "json" {
		_, err := fmt.Fprintln(c.App.Writer, text)
		return err
	}
	raw, err := json.Marshal(envelope{APIVersion: APIVersion, Kind: kind, Data: data})
	if err != nil {
		return oserr.Wrap(oserr.CodeInternal, err, "encoding %s result", kind)
	}
	_, err = fmt.Fprintf(c.App.Writer, "%s\n", raw)
	return err
}
