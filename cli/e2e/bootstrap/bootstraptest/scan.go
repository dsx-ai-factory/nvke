// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// The environment the lane configures the credential check through. Setting
// EnvKnownSecretsFile turns it on; a run without it is unchanged.
const (
	// EnvKnownSecretsFile names the file the run saves the secret values it
	// knows to, for the search at the end of the lane.
	EnvKnownSecretsFile = "NVKECTL_E2E_KNOWN_SECRETS_FILE"
	// EnvBootstrapCluster names the Kind cluster bootstrap site builds.
	EnvBootstrapCluster = "NVKECTL_E2E_BOOTSTRAP_CLUSTER"
	// EnvControllerName names the controller's Deployment, which is also its
	// namespace.
	EnvControllerName = "NVKECTL_E2E_CONTROLLER_NAME"
	// EnvGoTestLog names the file the Go test's own output goes to, which only
	// a later run can search.
	EnvGoTestLog = "NVKECTL_E2E_GO_TEST_LOG"
)

// clusterPollInterval is how often the run looks for the bootstrap cluster.
const clusterPollInterval = 5 * time.Second

// withheldLog replaces a bootstrap log that holds a known secret value or could
// not be searched for one, so a lane that prints the log prints this instead.
const withheldLog = "the bootstrap output held a known secret value, or could not be fully searched for one, " +
	"so it is not kept; the test log names why\n"

// SecretScan checks that a bootstrap run keeps its secrets out of its output. It
// gives nvkectl OTLP settings with a random token, learns the secrets bootstrap
// site generates, and searches the bootstrap log and the diagnostics for every
// value as an exact string.
type SecretScan struct {
	// OTLP are the settings the run gives nvkectl.
	OTLP OTLPSettings
	// Known is the set of values to search for.
	Known *SecretSet
	// KnownFile is where Known is saved.
	KnownFile string
	// LogFile is the bootstrap log, DiagnosticsDir the bundle, and KeyFile the
	// etcd key file.
	LogFile, DiagnosticsDir, KeyFile string
	// Controller is the controller's Deployment name and namespace.
	Controller string
	// Connect returns a client for the bootstrap cluster, and fails when it
	// cannot be reached.
	Connect func(ctx context.Context) (kubernetes.Interface, error)
	// PollInterval is how often Begin's watch looks for the cluster. Zero
	// means every five seconds.
	PollInterval time.Duration

	// clusterSeen is set once the bootstrap cluster has answered, so a later
	// failure to reach it is a failure to read, not a cluster never built.
	clusterSeen atomic.Bool
}

// NewSecretScanFromEnv returns the scan the lane asked for, or nil when
// EnvKnownSecretsFile is unset. It fails when a variable or an option the check
// needs is missing.
func NewSecretScanFromEnv(opts BootstrapOptions) (*SecretScan, error) {
	knownFile := os.Getenv(EnvKnownSecretsFile)
	if knownFile == "" {
		return nil, nil
	}
	cluster, controller := os.Getenv(EnvBootstrapCluster), os.Getenv(EnvControllerName)
	switch {
	case cluster == "":
		return nil, fmt.Errorf("%s is set but %s is not: name the bootstrap cluster", EnvKnownSecretsFile, EnvBootstrapCluster)
	case controller == "":
		return nil, fmt.Errorf("%s is set but %s is not: name the controller", EnvKnownSecretsFile, EnvControllerName)
	case opts.LogFile == "":
		return nil, fmt.Errorf("%s is set but %s is not: there is no log to search", EnvKnownSecretsFile, EnvBootstrapLog)
	case opts.EncryptionKeyOut == "":
		return nil, fmt.Errorf("%s is set but %s is not: there is no key file to read", EnvKnownSecretsFile, EnvEncryptionKeyOut)
	case opts.DiagnosticsDir == "":
		return nil, fmt.Errorf("%s is set but %s is not: there is no diagnostics bundle to search", EnvKnownSecretsFile, EnvDiagnosticsDir)
	}
	otlp, err := NewOTLPSettings()
	if err != nil {
		return nil, err
	}
	return &SecretScan{
		OTLP:           otlp,
		Known:          &SecretSet{},
		KnownFile:      knownFile,
		LogFile:        opts.LogFile,
		DiagnosticsDir: opts.DiagnosticsDir,
		KeyFile:        opts.EncryptionKeyOut,
		Controller:     controller,
		Connect:        KindCluster{Name: cluster}.Connect,
	}, nil
}

// Env returns the environment to start nvkectl with.
func (s *SecretScan) Env() []string { return s.OTLP.Env() }

// Begin saves the OTLP values and starts watching for the bootstrap cluster
// until ctx ends.
func (s *SecretScan) Begin(ctx context.Context) error {
	for _, v := range s.OTLP.Secrets() {
		s.Known.Add(v)
	}
	if err := s.Known.WriteFile(s.KnownFile); err != nil {
		return err
	}
	go s.watchCluster(ctx)
	return nil
}

func (s *SecretScan) watchCluster(ctx context.Context) {
	interval := s.PollInterval
	if interval <= 0 {
		interval = clusterPollInterval
	}
	// Checks at once, then every interval, until the cluster answers or ctx ends.
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		_, err := s.Connect(ctx)
		return err == nil, nil
	})
	if err == nil {
		s.clusterSeen.Store(true)
	}
}

// Finish reads the generated secrets, saves every known value, and searches the
// bootstrap log and the diagnostics for them. bootstrapErr is how bootstrap site
// ended, nil when it passed. A hit or an incomplete search replaces the log with
// a notice. Errors name files, never values.
func (s *SecretScan) Finish(ctx context.Context, bootstrapErr error) error {
	passed := bootstrapErr == nil
	kube, connErr := s.Connect(ctx)
	if connErr == nil {
		s.clusterSeen.Store(true)
	}

	var problems []error
	var incomplete []error
	for _, err := range s.learn(ctx, kube, connErr) {
		// A passing run needs every secret; a failed one only a secret that exists
		// and could not be read.
		if errors.Is(err, ErrUnreadable) || (passed && errors.Is(err, ErrNotGenerated)) {
			incomplete = append(incomplete, err)
			s.Known.MarkIncomplete(err.Error())
		}
	}
	if len(incomplete) > 0 {
		problems = append(problems, errors.New("the generated secrets could not be read, so the search for them proves nothing"))
		problems = append(problems, incomplete...)
	}
	if passed {
		if err := s.verifyController(ctx, kube, connErr); err != nil {
			problems = append(problems, err)
		}
	}
	if err := s.Known.WriteFile(s.KnownFile); err != nil {
		problems = append(problems, err)
	}

	hits, searchErr := s.Known.FindIn(s.LogFile, s.DiagnosticsDir)
	if searchErr != nil {
		problems = append(problems, searchErr)
	}
	if len(hits) > 0 {
		problems = append(problems, fmt.Errorf("a known secret value is in: %s", strings.Join(hits, ", ")))
	}
	// Not kept: the search was incomplete, the log holds a value, or it could not be searched.
	if len(incomplete) > 0 || searchErr != nil || slices.Contains(hits, s.LogFile) {
		problems = append(problems, s.withhold())
	}
	return errors.Join(problems...)
}

// learn adds the generated secrets it can read to Known and returns one error
// for each it could not, wrapping ErrNotGenerated or ErrUnreadable.
func (s *SecretScan) learn(ctx context.Context, kube kubernetes.Interface, connErr error) []error {
	var errs []error
	switch {
	case connErr == nil:
		password, err := PostgresPassword(ctx, kube)
		if err != nil {
			errs = append(errs, err)
		} else {
			s.Known.Add(password)
		}
	case s.clusterSeen.Load():
		errs = append(errs, fmt.Errorf("the Postgres password: %w: the bootstrap cluster was up and cannot be reached: %w", ErrUnreadable, connErr))
	default:
		errs = append(errs, fmt.Errorf("the Postgres password: %w", ErrNotGenerated))
	}

	values, err := EtcdKeyValues(s.KeyFile)
	if err != nil {
		errs = append(errs, err)
	}
	for _, v := range values {
		s.Known.Add(v)
	}
	return errs
}

func (s *SecretScan) verifyController(ctx context.Context, kube kubernetes.Interface, connErr error) error {
	if connErr != nil {
		return fmt.Errorf("checking the controller's OTLP headers: %w", connErr)
	}
	return VerifyControllerOTLPHeaders(ctx, kube, s.Controller, s.Controller, s.OTLP.ControllerHeaders())
}

// withhold replaces the bootstrap log with a notice, and returns the error that
// says so only when it could not.
func (s *SecretScan) withhold() error {
	if err := os.WriteFile(s.LogFile, []byte(withheldLog), 0o600); err != nil {
		return fmt.Errorf("could not replace %s, which may hold a secret: %w", s.LogFile, err)
	}
	return nil
}

// ScanKnownSecrets searches paths for the values saved in file and returns an
// error naming the files that hold one. It also fails on an incomplete saved
// set. The lane runs it at its end.
func ScanKnownSecrets(file string, paths ...string) error {
	known, err := LoadSecretSet(file)
	if err != nil {
		return err
	}
	if known.Len() == 0 {
		return fmt.Errorf("%s holds no secret values, so a clean search proves nothing", file)
	}
	var problems []error
	if reasons := known.Incomplete(); len(reasons) > 0 {
		problems = append(problems, fmt.Errorf("the search is incomplete, so a clean result proves nothing: %s", strings.Join(reasons, "; ")))
	}
	hits, err := known.FindIn(paths...)
	if err != nil {
		problems = append(problems, err)
	}
	if len(hits) > 0 {
		problems = append(problems, fmt.Errorf("a known secret value is in: %s", strings.Join(hits, ", ")))
	}
	return errors.Join(problems...)
}

// ScanKnownSecretsFromEnv runs ScanKnownSecrets over the bootstrap log, the Go
// test log and the diagnostics the lane names.
func ScanKnownSecretsFromEnv() error {
	file := os.Getenv(EnvKnownSecretsFile)
	if file == "" {
		return fmt.Errorf("%s is not set: name the file the run saved its secret values to", EnvKnownSecretsFile)
	}
	return ScanKnownSecrets(file, os.Getenv(EnvBootstrapLog), os.Getenv(EnvGoTestLog), os.Getenv(EnvDiagnosticsDir))
}
