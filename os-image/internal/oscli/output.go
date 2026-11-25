// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli

import (
	"encoding/json"
	"fmt"

	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
)

// formatFlag is the per-verb result format flag, as nvkr's verbs declare it.
func formatFlag() cli.Flag {
	return &cli.StringFlag{Name: "format", Value: "text", Usage: "text or json"}
}

// writeResult writes one result to stdout as text, or as result's flat JSON
// object when --format is json. Each JSON result carries its own
// schemaVersion field.
func writeResult(c *cli.Context, result any, text string) error {
	switch format := c.String("format"); format {
	case "text":
		_, err := fmt.Fprintln(c.App.Writer, text)
		return err
	case "json":
		raw, err := json.Marshal(result)
		if err != nil {
			return oserr.Wrap(oserr.CodeInternal, err, "encoding %s result", c.Command.Name)
		}
		_, err = fmt.Fprintf(c.App.Writer, "%s\n", raw)
		return err
	default:
		return oserr.New(oserr.CodeUsage, "--format must be text or json, got %q", format)
	}
}
