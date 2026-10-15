// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/publish"
	"github.com/dsx-ai-factory/nvke/release/internal/publish/testgrid"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func publishCmd() *cobra.Command {
	var (
		resultPath  string
		junitPath   string
		stream      string
		destination string
	)

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Ship one stream's qualification result to a destination",
		Long: `Ship one stream's qualification result to a destination.

Reads the Result at --result and the JUnit report at --junit and ships them
to --destination. testgrid is the only destination today; --destination
exists so a second one is additive rather than a rewrite.

If the Result is errored (Ginkgo never ran -- a Forge provisioning failure, a
wedged runner), this is a no-op: there is no suite name to tab the record
under, so nothing is published.

Does not read plan.json: the Result record already carries this leg's
Stream.Capabilities (gpu/kube and similar), stamped there by ` + "`qualify result`" + `,
so routing is built from the record itself.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := qualification.LoadResult(resultPath)
			if err != nil {
				return err
			}
			combo := result.Status.Combination
			if combo.Stream.Name != stream {
				return fmt.Errorf("--stream=%s does not match the result record's stream %q", stream, combo.Stream.Name)
			}

			if combo.Stream.Result == qualification.ResultErrored {
				fmt.Fprintf(cmd.OutOrStdout(), "skipping publish: %s/%s errored before Ginkgo ran, nothing to tab it under\n", combo.ID, stream)
				return nil
			}

			dest, closeDest, err := buildDestination(cmd.Context(), destination)
			if err != nil {
				return err
			}
			if closeDest != nil {
				defer closeDest()
			}

			if err := dest.Publish(cmd.Context(), result, junitPath); err != nil {
				return fmt.Errorf("publishing to %s: %w", dest.Name(), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "published %s/%s to %s\n", combo.ID, stream, dest.Name())
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&resultPath, "result", "", "path to the qualify-result.json this leg wrote (required)")
	f.StringVar(&junitPath, "junit", "", "path to the qualify-e2e.junit.xml this leg wrote, if any (required)")
	f.StringVar(&stream, "stream", "", "the stream name being published (required)")
	f.StringVar(&destination, "destination", "testgrid", "where to publish: testgrid")

	for _, name := range []string{"result", "junit", "stream"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// buildDestination constructs the named Destination. The returned func
// closes whatever the destination opened (a GCS client today); nil if there
// is nothing to close.
func buildDestination(ctx context.Context, name string) (publish.Destination, func(), error) {
	switch name {
	case "testgrid":
		uploader, err := testgrid.NewGCSUploader(ctx)
		if err != nil {
			return nil, nil, err
		}
		dest := &testgrid.Destination{Uploader: uploader}
		return dest, func() { _ = uploader.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown --destination=%s, have: testgrid", name)
	}
}
