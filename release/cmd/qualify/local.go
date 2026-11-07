// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/artifact"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// e2eScript holds every step of one stream's run. The workflow calls the
// same steps, so this command adds no logic of its own to a run.
const e2eScript = "release/hack/qualify-e2e.sh"

// siteStages are the -e2e.site-stage values the suite takes, checked here so
// that a typo fails before the images are built rather than in the suite.
var siteStages = []string{"provision", "bootstrap", "deployment", "full"}

// buildable is what the script can build from the checkout: each component's
// version variable, which the resolve step takes empty to mean "build it",
// and the plan entries the build stands in for, which verify then skips.
var buildable = []struct {
	name, version string
	planned       []string
}{
	{"controller", "SITE_NVKE_CONTROLLER_VERSION", []string{"controller"}},
	{"cli", "CLI_VERSION", []string{"cli", "cli-amd64", "cli-arm64"}},
	{"capi-provisioned", "CAPI_PROVISIONED_VERSION", []string{"capi-provisioned"}},
}

type localOptions struct {
	plan, scope, subject string
	version, overrides   string
	combination, stream  string
	stage, registry      string
	ngcOrg, nodeUser     string
	runDir               string
	build                []string
	fromSource           bool
	keepSite, keepTenant bool
	dryRun               bool
	wait                 time.Duration
}

func localCmd() *cobra.Command {
	var o localOptions
	cmd := &cobra.Command{
		Use:   "local",
		Short: "Run one stream on hardware from this checkout, as the workflow does",
		Long: `Run one stream on hardware from this checkout, as the workflow does.

The qualify-e2e workflow does not run for a fork's pull request, so a change
from a fork cannot be proven on hardware there. This runs the same lane from
your checkout, with your own credentials and Teleport login: it plans the run,
checks the artifacts, stages the site images, runs the suite on Forge
hardware, tears everything down, and records the result. Each step is a call
to ` + e2eScript + `, the script the workflow runs.

--build builds the named images from this checkout instead of pulling them.
--from-source builds all of them and pulls none, so no registry credential is
needed. The OS images are never built here.

--version and --overrides choose what is planned, as they do for qualify plan:
--version a tag other than the highest, and --overrides a published OS image
URL in place of one that is not published yet.

Everything the run writes is kept in --run-dir. Press Ctrl-C once to stop:
the node is torn down before the command exits.

The credentials and tools a run needs are listed in
docs/qualification/hardware-qualification.md.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLocal(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.plan, "plan", "", "the qualification plan to read, instead of planning --scope")
	f.StringVar(&o.scope, "scope", "full", "the scope to plan when --plan is not given")
	f.StringVar(&o.subject, "subject", "nvke", "the subject --scope plans")
	f.StringVar(&o.version, "version", "", "the version of the subject to plan (default: its highest version)")
	f.StringVar(&o.overrides, "overrides", "", "values to plan in place of what resolves, as name=value pairs separated by commas, as qualify plan takes them; not for a component --build builds")
	f.StringVar(&o.combination, "combination", "", "the combination to run (required if the plan has more than one)")
	f.StringVar(&o.stream, "stream", "", "the stream to run (required)")
	f.StringVar(&o.stage, "stage", "", "how far the site run goes: provision, bootstrap, deployment or full (required)")
	f.StringVar(&o.registry, "registry", "", "where the site images are pulled from: nvcr, ghcr, or local when every image is built (default: nvcr; local with --from-source)")
	f.StringVar(&o.ngcOrg, "ngc-org", "", "the NGC org the NVCR images are published under (default: $QUALIFY_E2E_NGC_ORG)")
	f.StringVar(&o.nodeUser, "node-user", "", "the account the OS image bakes (default: $QUALIFY_E2E_NODE_USER; empty is the OSS images')")
	f.StringSliceVar(&o.build, "build", nil, "components to build from the checkout instead of pulling: controller, cli, capi-provisioned")
	f.BoolVar(&o.fromSource, "from-source", false, "build every image the site stages from the checkout and pull none: --build for all of them, and --registry local")
	f.BoolVar(&o.keepSite, "keep-site", false, "keep the site node after the run, and a tenant whose specs failed; they must then be deleted by hand. The site node's SSH key is written under --run-dir")
	f.BoolVar(&o.keepTenant, "keep-tenant", false, "keep every tenant after the run, passed or not, and the site node they hang off; skips the spec that deletes the tenant, and they must then be deleted by hand. The site node's SSH key is written under --run-dir")
	f.BoolVar(&o.dryRun, "dry-run", false, "plan, verify, resolve and stage, and print the suite's arguments without taking a machine")
	f.DurationVar(&o.wait, "wait", 0, "how long to keep asking for an artifact that is not published yet, registry images and CDN files alike")
	f.StringVar(&o.runDir, "run-dir", "", "where the run's files are kept (default: a new directory under the user cache)")
	_ = cmd.MarkFlagRequired("stream")
	_ = cmd.MarkFlagRequired("stage")
	return cmd
}

// nodeKeyDir is where the suite writes the site node's SSH key: under the run
// directory when a node is kept, since nothing else can log into it after
// the run, and nowhere otherwise.
func nodeKeyDir(o localOptions) string {
	if !o.keepSite && !o.keepTenant {
		return ""
	}
	return filepath.Join(o.runDir, "node-keys")
}

func runLocal(ctx context.Context, out io.Writer, o localOptions) error {
	if o.ngcOrg == "" {
		o.ngcOrg = os.Getenv("QUALIFY_E2E_NGC_ORG")
	}
	// Planning expands the registry from the environment, and tsh reads its
	// proxy from it, so both are set here, before planning and the Teleport
	// checks, for every step to see the same values the script's steps do.
	if o.ngcOrg != "" {
		if err := os.Setenv("QUALIFY_E2E_NGC_ORG", o.ngcOrg); err != nil {
			return err
		}
	}
	if os.Getenv("TELEPORT_PROXY") == "" && os.Getenv("FORGE_TELEPORT_PROXY") != "" {
		if err := os.Setenv("TELEPORT_PROXY", os.Getenv("FORGE_TELEPORT_PROXY")); err != nil {
			return err
		}
	}
	if o.nodeUser == "" {
		o.nodeUser = os.Getenv("QUALIFY_E2E_NODE_USER")
	}
	if err := o.resolveBuild(); err != nil {
		return err
	}
	if !slices.Contains(siteStages, o.stage) {
		return fmt.Errorf("--stage %q: the stages are %s", o.stage, strings.Join(siteStages, ", "))
	}
	if err := o.checkPlanInputs(); err != nil {
		return err
	}
	for _, tool := range []string{"bash", "jq", "git", "make", "docker", "tsh"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not on PATH; the run needs it", tool)
		}
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	remote, err := resolve.UpstreamRemote(root)
	if err != nil {
		return err
	}
	if o.runDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		o.runDir = filepath.Join(cache, "nvke", "qualify-local", time.Now().Format("20060102-150405")+"-"+o.stream)
	}
	// The script's steps run from the repository root, not from where this was
	// started, so a relative path would name a different file to them.
	if o.runDir, err = filepath.Abs(o.runDir); err != nil {
		return err
	}
	if o.plan != "" {
		if o.plan, err = filepath.Abs(o.plan); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(o.runDir, 0o700); err != nil {
		return err
	}
	fmt.Fprintf(out, "run directory: %s\n", o.runDir)
	startedAt := time.Now().UTC().Format(time.RFC3339)

	planAt := o.plan
	if planAt == "" {
		planAt = filepath.Join(o.runDir, "plan.json")
		if err := planLocal(ctx, out, root, remote, planAt, o); err != nil {
			return err
		}
	}
	plan, err := qualification.LoadPlan(planAt)
	if err != nil {
		return err
	}
	if o.combination == "" && len(plan.Combinations) == 1 {
		o.combination = plan.Combinations[0].ID
	}
	combo, stream, err := qualification.CombinationStream(plan, o.combination, o.stream)
	if err != nil {
		return err
	}

	l := &steps{root: root, out: out, envFile: filepath.Join(o.runDir, "steps.env")}
	l.env = append(os.Environ(),
		"STEP_ENV="+l.envFile,
		"FETCH_REMOTE="+remote,
		"E2E_CONFIG="+filepath.Join(o.runDir, "e2e-config.yaml"),
		"SITE_INPUTS="+filepath.Join(o.runDir, "site-inputs"),
		"SITE_STAGE="+o.stage,
		"SITE_IMAGE_REGISTRY="+o.registry,
		"QUALIFY_E2E_NGC_ORG="+o.ngcOrg,
		"NODE_USER="+o.nodeUser,
		fmt.Sprintf("KEEP_SITE=%t", o.keepSite),
		fmt.Sprintf("KEEP_TENANT=%t", o.keepTenant),
		"NODE_KEY_DIR="+nodeKeyDir(o),
		fmt.Sprintf("DRY_RUN=%t", o.dryRun),
	)
	// The workflow takes these from repository variables and secrets, and
	// they are taken from the shell here. Each step refuses a name it reads
	// that is unset, so one added to the script and not here fails at once.
	for _, name := range []string{
		"NGC_API_KEY",
		"QUALIFY_E2E_SITE_CONTROLLER_VERSION",
		"SITE_DEPLOY_PATH_REF",
		"NVKE_CONTROLLER_NAMESPACE",
		"NVKE_CONTROLLER_SERVICE",
	} {
		l.env = append(l.env, name+"="+os.Getenv(name))
	}
	if err := os.WriteFile(l.envFile, nil, 0o600); err != nil {
		return err
	}
	// A --run-dir used before may hold an earlier run's report, which a run
	// that fails before the suite would otherwise record as its own.
	if err := os.Remove(filepath.Join(o.runDir, "qualify-e2e.report.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	ranSuite, runErr := runStream(ctx, out, o, l, planAt, combo, stream)
	if o.dryRun {
		return runErr
	}
	if err := recordLocal(ctx, out, o, planAt, l.envFile, combo.ID, stream.Name, startedAt, ranSuite); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

// runStream is everything after the stream is chosen, up to collecting what
// the suite wrote. It reports whether the suite was started, which is when
// the resolve step has recorded what was built.
func runStream(ctx context.Context, out io.Writer, o localOptions, l *steps, planAt string, combo *qualification.Combination, stream *qualification.Stream) (bool, error) {
	// Verify asks the CDN through the jump host, and the suite relays as
	// whoever is logged in, where it cannot prompt for a login that has
	// lapsed. A dry run verifies too, so it is checked either way.
	status := exec.CommandContext(ctx, "tsh", "status")
	if b, err := status.CombinedOutput(); err != nil {
		return false, fmt.Errorf("tsh status: %w: %s; run tsh login first", err, strings.TrimSpace(string(b)))
	}
	if err := verifyLocal(ctx, out, combo, stream, o.planned(), o.wait); err != nil {
		return false, err
	}

	if err := l.step("stream-env", planAt, combo.ID, stream.Name); err != nil {
		return false, err
	}
	// Built rather than pulled is what an empty version means to the
	// resolve step, as it does for a workflow run that names none.
	for _, b := range buildable {
		if slices.Contains(o.build, b.name) {
			if err := l.set(b.version, ""); err != nil {
				return false, err
			}
		}
	}
	names := []string{"resolve", "controller-image", "site-inputs"}
	if !o.dryRun {
		names = append(names, "config")
	}
	for _, s := range names {
		if err := l.step(s); err != nil {
			return false, err
		}
	}

	runErr := l.step("run")
	if err := l.step("withhold"); err != nil {
		return true, errors.Join(runErr, err)
	}
	if err := l.collect(o.runDir); err != nil {
		return true, errors.Join(runErr, err)
	}
	return true, runErr
}

// recordLocal writes qualify-result.json for a run that got as far as
// choosing its stream, as the workflow's record step does whatever failed:
// errored when the suite wrote no report. Before the suite started, a built
// component may have no recorded version yet, and is named as built with none.
func recordLocal(ctx context.Context, out io.Writer, o localOptions, planAt, envFile, combo, stream, startedAt string, ranSuite bool) error {
	resultPlan := planAt
	if len(o.build) > 0 {
		resultPlan = filepath.Join(o.runDir, "plan-as-run.json")
		if err := writeAsBuilt(planAt, resultPlan, envFile, o.build, combo, stream, ranSuite); err != nil {
			return err
		}
	}
	result := resultCmd()
	result.SetOut(out)
	result.SetArgs([]string{
		"--plan", resultPlan,
		"--report", filepath.Join(o.runDir, "qualify-e2e.report.json"),
		"--combination", combo,
		"--stream", stream,
		"--started-at", startedAt,
		"--output", filepath.Join(o.runDir, "qualify-result.json"),
	})
	if err := result.ExecuteContext(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	fmt.Fprintf(out, "result: %s\n", filepath.Join(o.runDir, "qualify-result.json"))
	return nil
}

// planLocal plans the way the workflow's plan job does, after fetching
// upstream's tags so the versions are named by the tags the publishers see.
// The fetch prunes because the hash length counts every object reachable from
// the upstream remote's branches, and a branch deleted upstream would still
// count in a long-lived clone.
func planLocal(ctx context.Context, out io.Writer, root, remote, planAt string, o localOptions) error {
	fmt.Fprintf(out, "fetching %s's tags\n", remote)
	fetch := exec.CommandContext(ctx, "git", "fetch", "--quiet", "--prune", "--tags", remote)
	fetch.Dir, fetch.Stdout, fetch.Stderr = root, out, out
	if err := fetch.Run(); err != nil {
		return fmt.Errorf("fetching %s's tags: %w", remote, err)
	}
	plan := planCmd()
	plan.SetOut(out)
	plan.SetArgs([]string{"--subject", o.subject, "--scope", o.scope, "--version", o.version, "--overrides", o.overrides, "--root", root, "--output", planAt})
	if err := plan.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("planning: %w", err)
	}
	return nil
}

// checkPlanInputs refuses what planning would ignore: --version and
// --overrides with a plan that is read rather than made, and an override of a
// component this run builds, whose version is the commit whatever the plan says.
func (o *localOptions) checkPlanInputs() error {
	if o.plan != "" && (o.version != "" || o.overrides != "") {
		return errors.New("--version and --overrides choose what is planned, and --plan skips planning; give one or the other")
	}
	chosen, err := qualification.ParseOverrides(o.overrides)
	if err != nil {
		return fmt.Errorf("--overrides: %w", err)
	}
	built := o.planned()
	for _, name := range slices.Sorted(maps.Keys(chosen)) {
		if slices.Contains(built, name) {
			return fmt.Errorf("--overrides %s: %s is built from this checkout, so its version is the commit; drop it from --build or from --overrides", name, name)
		}
	}
	return nil
}

// resolveBuild checks --build and applies --from-source and the registry
// default, so the rest of the run reads one settled set.
func (o *localOptions) resolveBuild() error {
	names := make([]string, 0, len(buildable))
	for _, b := range buildable {
		names = append(names, b.name)
	}
	for _, b := range o.build {
		if !slices.Contains(names, b) {
			return fmt.Errorf("--build %q: only %s are built here", b, strings.Join(names, ", "))
		}
	}
	if o.fromSource {
		o.build = names
		if o.registry == "" {
			o.registry = "local"
		}
		if o.registry != "local" {
			return fmt.Errorf("--from-source pulls nothing, so --registry %s has nothing to do; leave it unset", o.registry)
		}
	}
	if o.registry == "" {
		o.registry = "nvcr"
	}
	return nil
}

func subset(names, of []string) bool {
	for _, n := range names {
		if !slices.Contains(of, n) {
			return false
		}
	}
	return true
}

// planned is the plan entries the run builds instead of pulling.
func (o *localOptions) planned() []string {
	var out []string
	for _, b := range buildable {
		if slices.Contains(o.build, b.name) {
			out = append(out, b.planned...)
		}
	}
	return out
}

// verifyLocal is qualify verify for the one stream, except that the CDN is
// asked from the site: it refuses a workstation with the 403 it gives for
// something that is not there. A location holding only what the run builds
// is not asked about, since nothing is pulled from it.
func verifyLocal(ctx context.Context, out io.Writer, combo *qualification.Combination, stream *qualification.Stream, built []string, wait time.Duration) error {
	one := &qualification.Plan{Combinations: []qualification.Combination{*combo}}
	one.Combinations[0].Streams = []qualification.Stream{*stream}

	var registry, web []artifact.Location
	for _, l := range locations(one) {
		if subset(l.Wanted, built) {
			fmt.Fprintf(out, "built here, not asked for: %s (%s)\n", l.URL, strings.Join(l.Wanted, ", "))
			continue
		}
		if strings.HasPrefix(l.URL, "https://") {
			web = append(web, l)
		} else {
			registry = append(registry, l)
		}
	}
	var errs []error
	deadline := time.Now().Add(wait)
	checker := artifact.Checker{Auth: artifact.EnvAuth(registryCredentials...)}
	if len(registry) > 0 {
		if err := checker.WaitFor(ctx, out, registry, wait, artifact.DefaultPoll); err != nil {
			errs = append(errs, err)
		}
	}
	if len(web) > 0 {
		hostVar, host, login := jumpHost()
		if host == "" {
			return errors.Join(append(errs, errors.New("the CDN answers only the sites; set FORGE_JUMP_HOST to ask it from there"))...)
		}
		fmt.Fprintf(out, "asking the CDN from the site, through $%s\n", hostVar)
		ask := func(ctx context.Context, url string) (string, error) {
			code, err := fromSite(ctx, host, login, url)
			if err != nil {
				err = errors.New(strings.ReplaceAll(err.Error(), host, "$"+hostVar))
			}
			return code, err
		}
		if err := askCDN(ctx, out, web, ask, time.Until(deadline), artifact.DefaultPoll); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// askCDN asks for each web location through ask, and again every poll for
// the ones not yet published, until all are or wait has passed, as
// artifact.Checker.WaitFor does for registry locations.
func askCDN(ctx context.Context, out io.Writer, web []artifact.Location, ask func(context.Context, string) (string, error), wait, poll time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var pending []artifact.Location
		var missing []string
		for _, l := range web {
			code, err := ask(ctx, l.URL)
			if err == nil && strings.HasPrefix(code, "2") {
				fmt.Fprintf(out, "published: %s (%s)\n", l.URL, strings.Join(l.Wanted, ", "))
				continue
			}
			reason := "HTTP " + code
			if err != nil {
				reason = err.Error()
			}
			pending = append(pending, l)
			missing = append(missing, fmt.Sprintf("  %s (%s): %s from the site", l.URL, strings.Join(l.Wanted, ", "), reason))
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().Add(poll).After(deadline) {
			return fmt.Errorf("%d of the CDN artifacts the plan names are not published:\n%s", len(missing), strings.Join(missing, "\n"))
		}
		fmt.Fprintf(out, "%d CDN artifacts not published yet; asking again in %s\n", len(pending), poll)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		web = pending
	}
}

// jumpHost is the first site jump host set in the environment, with the
// variable that named it and the Teleport login for it. Any site reaches the
// CDN, so which one does not matter.
func jumpHost() (hostVar, host, login string) {
	for _, s := range [][2]string{
		{"FORGE_JUMP_HOST", "FORGE_TELEPORT_LOGIN"},
		{"FORGE_SITE_2_JUMP_HOST", "FORGE_SITE_2_TELEPORT_LOGIN"},
		{"DSX_DATAHALL_1_JUMP_HOST", "DSX_DATAHALL_1_TELEPORT_LOGIN"},
	} {
		if h := os.Getenv(s[0]); h != "" {
			return s[0], h, os.Getenv(s[1])
		}
	}
	return "", "", ""
}

// fromSite is the HTTP status the jump host gets for a HEAD of url.
func fromSite(ctx context.Context, host, login, url string) (string, error) {
	if strings.ContainsAny(url, "'\\") {
		return "", fmt.Errorf("refusing to quote %q for a remote shell", url)
	}
	target := host
	if login != "" {
		target = login + "@" + host
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "tsh", "ssh", target,
		"curl -s -o /dev/null -w '%{http_code}' -I --max-time 30 '"+url+"'")
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return strings.TrimSpace(string(b)), err
}

// steps runs the steps of qualify-e2e.sh, each with what the earlier ones
// resolved, as the workflow's steps read $GITHUB_ENV.
type steps struct {
	root, envFile string
	env           []string
	out           io.Writer
}

func (l *steps) set(name, value string) error {
	f, err := os.OpenFile(l.envFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s=%s\n", name, value)
	return err
}

// step runs one step. The terminal's interrupt reaches it directly, except
// for the suite: that runs in its own process group, so it is interrupted
// once, from here, and gets to tear the node down. Ginkgo skips its cleanup
// on a second interrupt, and an instance abandoned mid-teardown leaks.
func (l *steps) step(args ...string) error {
	resolved, err := readEnvFile(l.envFile)
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", append([]string{e2eScript}, args...)...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = l.root, l.out, l.out
	cmd.Env = append(slices.Clone(l.env), resolved...)
	isolated := args[0] == "run"
	if isolated {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.Stdin = os.Stdin
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	interrupted := false
	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if interrupted {
				return fmt.Errorf("%s: interrupted", args[0])
			}
			return nil
		case <-signals:
			if !isolated {
				interrupted = true
				_ = cmd.Process.Signal(os.Interrupt)
				continue
			}
			if interrupted {
				fmt.Fprintf(l.out, "\nstill tearing down; kill -9 %d abandons the node, which then has to be released by hand\n", cmd.Process.Pid)
				continue
			}
			interrupted = true
			fmt.Fprintln(l.out, "\ninterrupted; the suite is tearing the node down")
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		}
	}
}

// collect moves what the suite wrote into dir, after withhold.
func (l *steps) collect(dir string) error {
	cmd := exec.Command("bash", e2eScript, "artifacts")
	cmd.Dir, cmd.Stderr = l.root, l.out
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	for _, path := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if path == "" {
			continue
		}
		if err := move(path, filepath.Join(dir, filepath.Base(path))); err != nil {
			return err
		}
	}
	return nil
}

var rename = os.Rename

// move renames src to dst, or copies and removes it where the two are on
// different volumes, as a --run-dir on another disk is. Whatever dst holds,
// from an earlier run in a reused --run-dir, is replaced.
func move(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	err = rename(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if info.IsDir() {
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// builtStrategy is the version strategy recorded for a component built from
// the checkout, where the plan would name a resolver.
const builtStrategy = "builtFromCheckout"

// writeAsBuilt copies the plan to outAt with each built component's entries
// naming the version the run built, which the resolve step recorded, and no
// location, since nothing was pulled. The result is written from it, so it
// records what ran rather than what the plan would have pulled. Only the
// combination and stream that ran are rewritten; the rest stay as planned.
// requireVersions refuses a built component with no recorded version, which
// a run that started the suite always has.
func writeAsBuilt(planAt, outAt, envFile string, built []string, combo, stream string, requireVersions bool) error {
	resolved, err := readEnvFile(envFile)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, kv := range resolved {
		k, v, _ := strings.Cut(kv, "=")
		values[k] = v
	}
	versions := map[string]string{}
	for _, b := range buildable {
		if !slices.Contains(built, b.name) {
			continue
		}
		v := values[b.version]
		if v == "" && requireVersions {
			return fmt.Errorf("the run built %s but recorded no %s", b.name, b.version)
		}
		for _, w := range b.planned {
			versions[w] = v
		}
	}
	p, err := qualification.LoadPlan(planAt)
	if err != nil {
		return err
	}
	asBuilt := func(entries []qualification.ComponentVersion) {
		for i := range entries {
			if v, ok := versions[entries[i].Name]; ok {
				entries[i] = qualification.ComponentVersion{Name: entries[i].Name, Version: v, VersionStrategy: builtStrategy}
			}
		}
	}
	for i := range p.Combinations {
		c := &p.Combinations[i]
		if c.ID != combo {
			continue
		}
		asBuilt(c.Components)
		asBuilt(c.Dependencies)
		for j := range c.Streams {
			if c.Streams[j].Name == stream {
				asBuilt(c.Streams[j].Dependencies)
			}
		}
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outAt, append(b, '\n'), 0o600)
}

// readEnvFile reads the NAME=value lines the steps wrote, later ones last so
// that they win.
func readEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var env []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.Contains(s.Text(), "=") {
			env = append(env, s.Text())
		}
	}
	return env, s.Err()
}
