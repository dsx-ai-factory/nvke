// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/publish"
	"github.com/dsx-ai-factory/nvke/release/internal/publish/testgrid"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func publishCmd() *cobra.Command {
	var (
		resultPath  string
		junitPath   string
		combination string
		stream      string
		destination string
		bucket      string
	)

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Ship one stream's qualification result to a destination",
		Long: `Ship one stream's qualification result to a destination.

Reads the Result at --result and the JUnit report at --junit and ships them
to --destination. testgrid is the only destination today; --destination
exists so a second one is additive rather than a rewrite.

Does not read plan.json: the Result record already carries this leg's
Stream.Capabilities (gpu/kube and similar), stamped there by ` + "`qualify result`" + `,
so routing is built from the record itself.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := loadResult(resultPath)
			if err != nil {
				return err
			}
			if len(result.Status.Combinations) != 1 {
				return fmt.Errorf("%s: has %d combinations, want exactly 1", resultPath, len(result.Status.Combinations))
			}
			combo := result.Status.Combinations[0]
			if len(combo.Streams) != 1 {
				return fmt.Errorf("%s: combination %q has %d streams, want exactly 1", resultPath, combo.ID, len(combo.Streams))
			}
			if combo.ID != combination {
				return fmt.Errorf("--combination=%s does not match the result record's combination %q", combination, combo.ID)
			}
			if combo.Streams[0].Name != stream {
				return fmt.Errorf("--stream=%s does not match the result record's stream %q", stream, combo.Streams[0].Name)
			}
			if _, err := os.Stat(junitPath); err != nil {
				return fmt.Errorf("JUnit report: %w", err)
			}

			dest, closeDest, err := buildDestination(cmd.Context(), destination, bucket)
			if err != nil {
				return err
			}
			if closeDest != nil {
				defer closeDest()
			}

			if err := dest.Publish(cmd.Context(), result, junitPath); err != nil {
				return fmt.Errorf("publishing to %s: %w", dest.Name(), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "published %s/%s to %s\n", combination, stream, dest.Name())
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&resultPath, "result", "", "path to the qualify-result.json this leg wrote (required)")
	f.StringVar(&junitPath, "junit", "", "path to the qualify-e2e.junit.xml this leg wrote (required)")
	f.StringVar(&combination, "combination", "", "the combination id being published (required)")
	f.StringVar(&stream, "stream", "", "the stream name being published (required)")
	f.StringVar(&destination, "destination", "testgrid", "where to publish: testgrid")
	f.StringVar(&bucket, "bucket", "dgxc-testgrid", "the GCS bucket testgrid publishes to")

	for _, name := range []string{"result", "junit", "combination", "stream"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// loadResult reads a QualificationResult written by `qualify result`.
func loadResult(path string) (*qualification.Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading result record: %w", err)
	}
	var result qualification.Result
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, fmt.Errorf("parsing result record: %w", err)
	}
	if result.Kind != qualification.KindQualificationResult {
		return nil, fmt.Errorf("%s: kind is %q, want %q", path, result.Kind, qualification.KindQualificationResult)
	}
	return &result, nil
}

// buildDestination constructs the named Destination. The returned func
// closes whatever the destination opened (a GCS client today); nil if there
// is nothing to close.
func buildDestination(ctx context.Context, name, bucket string) (publish.Destination, func(), error) {
	switch name {
	case "testgrid":
		uploader, err := testgrid.NewGCSUploader(ctx, bucket)
		if err != nil {
			return nil, nil, err
		}
		dest := &testgrid.Destination{Uploader: uploader}
		return dest, func() { _ = uploader.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown --destination=%s, have: testgrid", name)
	}
}
