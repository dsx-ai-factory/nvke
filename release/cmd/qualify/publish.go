// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/publish"
	"github.com/dsx-ai-factory/nvke/release/internal/publish/testgrid"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// publishTimeout bounds the GCS calls this command makes -- not the whole
// process the way main's root context used to. plan and verify both need
// the opposite of a short bound (verify's own --wait can run up to 90m in
// CI, see qualify-e2e.yml), so this is scoped to publish's RunE instead of
// wrapping root().ExecuteContext(), which would cut verify's wait off at two
// minutes regardless of what --wait asked for. Two minutes is well above any
// realistic publish, which uploads at most three small files with its own
// bounded retry/backoff.
const publishTimeout = 2 * time.Minute

// testGridBucketEnvVar names the bucket qualify publish writes to. Read from
// the environment rather than baked in as a literal: the bucket this
// pipeline writes to has already moved once (DGXC's own TestGrid to AICR's),
// a compiled-in name means every future move needs a new build instead of a
// workflow env change, and this repository is headed for a public mirror --
// an internal bucket name does not belong in source there even though it is
// not itself a secret.
const testGridBucketEnvVar = "TESTGRID_BUCKET"

// resolveBucket reads testGridBucketEnvVar, erroring if it is unset or
// empty rather than falling back to a compiled-in default -- see
// testGridBucketEnvVar's own doc for why there is no default to fall back
// to.
func resolveBucket() (string, error) {
	b := os.Getenv(testGridBucketEnvVar)
	if b == "" {
		return "", fmt.Errorf("%s is not set", testGridBucketEnvVar)
	}
	return b, nil
}

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
exists so a second one is additive rather than a rewrite. Which bucket it
writes to is read from ` + "`" + testGridBucketEnvVar + "`" + `, not a flag or a literal --
this command errors if it is unset, rather than falling back to a bucket
name baked into source.

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

			bucket, err := resolveBucket()
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), publishTimeout)
			defer cancel()

			dest, closeDest, err := buildDestination(ctx, destination, bucket)
			if err != nil {
				return err
			}
			if closeDest != nil {
				defer closeDest()
			}

			if err := dest.Publish(ctx, result, junitPath); err != nil {
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

// buildDestination constructs the named Destination, writing to bucket. The
// returned func closes whatever the destination opened (a GCS client
// today); nil if there is nothing to close.
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
