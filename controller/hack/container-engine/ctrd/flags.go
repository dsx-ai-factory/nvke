// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

type flagKind int

const (
	boolFlag flagKind = iota
	stringFlag
	listFlag
)

// flagSpec describes one flag a command accepts. Short is its one-letter form.
// Some accepted flags have no effect, because they ask for what this engine does
// anyway; the command simply never reads them.
type flagSpec struct {
	Kind  flagKind
	Short string
}

type flagSet map[string]flagSpec

// parsedFlags is the result of parsing one command line.
type parsedFlags struct {
	bools map[string]bool
	strs  map[string]string
	lists map[string][]string
	args  []string
}

func (p *parsedFlags) str(name string) string         { return p.strs[name] }
func (p *parsedFlags) list(name string) []string      { return p.lists[name] }
func (p *parsedFlags) has(name string) bool           { return p.bools[name] }
func (p *parsedFlags) set(name string) (string, bool) { value, ok := p.strs[name]; return value, ok }

// notSupportedError is what a flag or command this engine does not implement
// reports.
type notSupportedError string

func (e notSupportedError) Error() string { return string(e) + ": not supported by this engine" }

// parseFlags parses args against the flags of one command. With stopAtArg the
// first argument that is not a flag ends the flags, the way `docker run` and
// `docker exec` end them at the image or container; otherwise flags and
// arguments may be mixed, the way the other commands allow.
func parseFlags(command string, specs flagSet, args []string, stopAtArg bool) (*parsedFlags, error) {
	parsed := &parsedFlags{bools: map[string]bool{}, strs: map[string]string{}, lists: map[string][]string{}}
	shorts := map[string]string{}
	for name, spec := range specs {
		if spec.Short != "" {
			shorts[spec.Short] = name
		}
	}
	lookup := func(arg string) (string, flagSpec, error) {
		name := shorts[arg[1:]]
		if strings.HasPrefix(arg, "--") {
			name = arg[2:]
		}
		spec, ok := specs[name]
		if !ok {
			return "", spec, notSupportedError(command + " " + arg)
		}
		return name, spec, nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			parsed.args = append(parsed.args, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			parsed.args = append(parsed.args, arg)
			if stopAtArg {
				parsed.args = append(parsed.args, args[i+1:]...)
				break
			}
			continue
		}
		flag, value, hasValue := strings.Cut(arg, "=")
		// -aq is -a -q: a run of one-letter boolean flags.
		if !strings.HasPrefix(flag, "--") && len(flag) > 2 && !hasValue {
			for _, letter := range flag[1:] {
				name, spec, err := lookup("-" + string(letter))
				if err != nil || spec.Kind != boolFlag {
					return nil, notSupportedError(command + " " + arg)
				}
				parsed.bools[name] = true
			}
			continue
		}
		name, spec, err := lookup(flag)
		if err != nil {
			return nil, err
		}
		if spec.Kind == boolFlag {
			parsed.bools[name] = !hasValue || value != "false"
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s: flag %s needs an argument", command, flag)
			}
			i++
			value = args[i]
		}
		if spec.Kind == listFlag {
			parsed.lists[name] = append(parsed.lists[name], value)
		} else {
			parsed.strs[name] = value
		}
	}
	return parsed, nil
}
