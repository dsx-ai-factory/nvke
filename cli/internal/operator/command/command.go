// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package command holds the nvkectl subcommands that manage machine images and
// releases on a site's management cluster.
package command

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/operator"
	"github.com/dsx-ai-factory/nvke/cli/internal/util"
)

// ClientFn builds the operator Client for the active site.
//
// Commands take this rather than a built client so the site is only looked up
// when a command actually runs, leaving --help, and a client-side --dry-run,
// working on a machine with no site configured at all.
type ClientFn func() (operator.Client, error)

// kindInfo is how one machine image or release kind appears on the command line.
type kindInfo struct {
	kind operator.Kind
	// use is the singular lower-case command name, such as "machineimage".
	use string
	// noun is how messages describe one resource of the kind.
	noun string
	// what is a short description of the kind for help text.
	what string
	// usedBy says what may still depend on a resource of the kind, for the
	// note printed after a delete.
	usedBy string
}

// kindInfos lists the kinds in the order their commands are registered.
var kindInfos = []kindInfo{
	{
		operator.KindMachineImage, "machineimage", "machine image",
		"a machine image: an immutable node image and its NICo boot script",
		"a cluster release or node pool release still names",
	},
	{
		operator.KindClusterRelease, "clusterrelease", "cluster release",
		"a cluster release: a control-plane osProfile",
		"a cluster still selects",
	},
	{
		operator.KindNodePoolRelease, "nodepoolrelease", "node pool release",
		"a node pool release: a worker osProfile",
		"a node pool still selects",
	},
}

// plural returns the plural command name of the kind.
func (k kindInfo) plural() string { return k.use + "s" }

// readDocument reads a request document from path, or from stdin when path is "-".
func readDocument(cmd *cobra.Command, path string) ([]byte, error) {
	return util.ReadFileOrStdin(cmd.InOrStdin(), path, "request")
}

// notFoundError reports the names that matched nothing, or returns nil.
func notFoundError(k kindInfo, missing []string) error {
	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%s %q not found", k.noun, missing[0])
	default:
		return fmt.Errorf("%ss not found: %s", k.noun, strings.Join(missing, ", "))
	}
}

// isNotFound reports whether err says the named resource does not exist.
func isNotFound(err error) bool {
	return errors.Is(err, operator.ErrNotFound)
}
