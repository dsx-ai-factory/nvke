// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
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

// testGridBuckets is the TestGrid bucket for each environment --environment
// picks, not a raw bucket name a typo could point anywhere. "prod" is the
// one real TestGrid actually reads from today; "staging" exists for a
// caller who wants to inspect what would have been published without
// touching prod, not because anything renders it -- config-gen's staging
// instance reads its raw results from the *prod* bucket too (its own
// --source-bucket), so a staging publish is visible only via
// `gcloud storage ls`, never on any TestGrid dashboard.
var testGridBuckets = map[string]string{
	"staging": "dgxc-testgrid-staging",
	"prod":    "dgxc-testgrid",
}

func publishCmd() *cobra.Command {
	var (
		resultPath  string
		junitPath   string
		stream      string
		destination string
		environment string
	)

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Ship one stream's qualification result to a destination",
		Long: `Ship one stream's qualification result to a destination.

Reads the Result at --result and the JUnit report at --junit and ships them
to --destination. testgrid is the only destination today; --destination
exists so a second one is additive rather than a rewrite. --environment
picks which bucket within that destination: prod (the default, and the only
one any TestGrid dashboard actually reads) or staging (visible only via
` + "`gcloud storage ls`" + `, not any dashboard -- see testGridBuckets).

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

			bucket, ok := testGridBuckets[environment]
			if !ok {
				return fmt.Errorf("unknown --environment=%s, have: staging, prod", environment)
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
	f.StringVar(&environment, "environment", "prod", "which bucket to publish to: prod or staging (staging renders on no TestGrid dashboard)")

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
