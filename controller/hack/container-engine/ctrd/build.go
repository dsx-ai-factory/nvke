// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// buildctlArgs is the buildctl command line for `build`: the Dockerfile frontend
// against the running buildkitd. Without --output the image goes into the
// containerd namespace buildkitd's containerd worker is configured with, which
// has to be the engine's own (the preflight checks it).
func buildctlArgs(host, context, dockerfile, platform string, tags, buildArgs []string, output string) []string {
	args := []string{
		"--addr", host, "build", "--frontend", "dockerfile.v0",
		"--local", "context=" + context,
		"--local", "dockerfile=" + filepath.Dir(dockerfile),
		"--opt", "filename=" + filepath.Base(dockerfile),
	}
	if platform != "" {
		args = append(args, "--opt", "platform="+platform)
	}
	for _, arg := range buildArgs {
		args = append(args, "--opt", "build-arg:"+arg)
	}
	names := strings.Join(tags, ",")
	switch {
	case output == "" && names != "":
		args = append(args, "--output", `type=image,"name=`+names+`",unpack=true`)
	case output != "":
		// buildkit's docker and oci exporters want the name docker gets from --tag.
		if !strings.Contains(output, "name=") && names != "" {
			output += `,"name=` + names + `"`
		}
		args = append(args, "--output", output)
	}
	return args
}

func (e *engine) build(args []string) error {
	flags, err := parseFlags("build", flagSet{
		"tag": {Kind: listFlag, Short: "t"}, "file": {Kind: stringFlag, Short: "f"}, flagPlatform: {Kind: stringFlag},
		"build-arg": {Kind: listFlag}, "output": {Kind: stringFlag, Short: "o"},
	}, args, false)
	if err != nil {
		return err
	}
	if len(flags.args) != 1 {
		return errors.New("build: one build context is required")
	}
	context, err := filepath.Abs(flags.args[0])
	if err != nil {
		return err
	}
	dockerfile := flags.str("file")
	if dockerfile == "" {
		dockerfile = filepath.Join(context, "Dockerfile")
	}
	if dockerfile, err = filepath.Abs(dockerfile); err != nil {
		return err
	}
	var tags, buildArgs []string
	for _, tag := range flags.list("tag") {
		tags = append(tags, normalizeRef(tag))
	}
	for _, arg := range flags.list("build-arg") {
		if _, _, hasValue := strings.Cut(arg, "="); hasValue {
			buildArgs = append(buildArgs, arg)
		} else if value, ok := os.LookupEnv(arg); ok {
			buildArgs = append(buildArgs, arg+"="+value)
		}
	}
	cmd := e.command("buildctl", buildctlArgs(e.cfg.BuildkitHost, context, dockerfile, flags.str(flagPlatform), tags, buildArgs, flags.str("output"))...)
	cmd.Stdout = e.stderr
	return cmd.Run()
}
