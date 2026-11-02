// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/etcdkey"
)

// The generated secrets bootstrap site makes, and where it puts them. The
// Postgres password is in the chart's Secret; the etcd key is in the file named
// by --encryption-key-out.
const (
	postgresNamespace   = "postgres"
	postgresSecretName  = "postgres-postgresql"
	postgresPasswordKey = "password"

	// otlpHeadersEnv is the controller's environment variable that carries the
	// OTLP headers.
	otlpHeadersEnv = "OTEL_EXPORTER_OTLP_HEADERS"
)

var (
	// ErrNotGenerated reports a secret that does not exist yet, as when
	// bootstrap failed before it made one.
	ErrNotGenerated = errors.New("not generated yet")
	// ErrUnreadable reports a secret that exists, or may, and could not be
	// read.
	ErrUnreadable = errors.New("could not be read")
)

// printableKey matches a decoded etcd key that is text, the form it could
// appear in as a log field.
var printableKey = regexp.MustCompile(`^[[:print:]]{16,}$`)

// PostgresPassword reads the password bootstrap site generated for Postgres.
// It returns ErrNotGenerated when the Secret is not in the cluster, and
// ErrUnreadable for any other failure, including a Secret with no password.
func PostgresPassword(ctx context.Context, kube kubernetes.Interface) (string, error) {
	secret, err := kube.CoreV1().Secrets(postgresNamespace).Get(ctx, postgresSecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "", fmt.Errorf("the Postgres password: %w", ErrNotGenerated)
	case err != nil:
		return "", fmt.Errorf("the Postgres password: %w: %w", ErrUnreadable, err)
	}
	password := secret.Data[postgresPasswordKey]
	if len(password) == 0 {
		return "", fmt.Errorf("the Postgres password: %w: Secret %s/%s has no %s",
			ErrUnreadable, postgresNamespace, postgresSecretName, postgresPasswordKey)
	}
	return string(password), nil
}

// EtcdKeyValues reads the etcd encryption key from the file bootstrap site
// wrote it to, and returns it as stored (base64) and, when it is text, decoded.
// It returns ErrNotGenerated when the file does not exist, and ErrUnreadable
// when it does and holds no key.
func EtcdKeyValues(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("the etcd key: %w", ErrNotGenerated)
	case err != nil:
		return nil, fmt.Errorf("the etcd key: %w: %w", ErrUnreadable, err)
	}
	var manifest struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("the etcd key: %w: %s is not a Secret manifest", ErrUnreadable, path)
	}
	stored := manifest.Data[etcdkey.FieldActive]
	if stored == "" {
		return nil, fmt.Errorf("the etcd key: %w: %s has no %s key", ErrUnreadable, path, etcdkey.FieldActive)
	}
	values := []string{stored}
	// The key is random bytes, so it can only appear in output as text if every
	// byte prints.
	if decoded, err := base64.StdEncoding.DecodeString(stored); err == nil && printableKey.Match(decoded) {
		values = append(values, string(decoded))
	}
	return values, nil
}

// VerifyControllerOTLPHeaders checks that the controller's Deployment carries
// OTEL_EXPORTER_OTLP_HEADERS set to want. The error holds neither value.
func VerifyControllerOTLPHeaders(ctx context.Context, kube kubernetes.Interface, namespace, name, want string) error {
	deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading Deployment %s/%s: %w", namespace, name, err)
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == otlpHeadersEnv && env.Value == want {
				return nil
			}
		}
	}
	return fmt.Errorf("the controller's %s is not the headers the run gave bootstrap site, "+
		"so the search for them proves nothing about the controller's output", otlpHeadersEnv)
}

// KindCluster is the ephemeral Kind cluster bootstrap site builds, which the
// lane keeps so the run can read it afterwards.
type KindCluster struct {
	// Name is the cluster's name.
	Name string
	// Run runs kind with args and returns its standard output. It defaults to
	// the kind in PATH, selecting the nerdctl provider as bootstrap site does.
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

// Connect returns a client for the cluster's API. It fails when kind cannot
// return the cluster's kubeconfig, which includes the cluster not existing, and
// when the API server does not answer.
func (c KindCluster) Connect(ctx context.Context) (kubernetes.Interface, error) {
	run := c.Run
	if run == nil {
		run = runKind
	}
	kubeconfig, err := run(ctx, "get", "kubeconfig", "--name", c.Name)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig of kind cluster %s: %w", c.Name, err)
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing the kubeconfig of kind cluster %s: %w", c.Name, err)
	}
	// Bounded, so a cluster that stopped answering fails the read rather than
	// hanging the run.
	config.Timeout = 10 * time.Second
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating a client for kind cluster %s: %w", c.Name, err)
	}
	// kind returns a kubeconfig as soon as the node exists, before the API
	// server is up, and a cluster is only worth reading once it answers.
	if _, err := kube.Discovery().ServerVersion(); err != nil {
		return nil, fmt.Errorf("the API server of kind cluster %s did not answer: %w", c.Name, err)
	}
	return kube, nil
}

func runKind(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kind", args...)
	cmd.Env = append(os.Environ(), "KIND_EXPERIMENTAL_PROVIDER=nerdctl")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kind %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
