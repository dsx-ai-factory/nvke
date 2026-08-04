// Package k8s provides Kubernetes client helpers for the node agent.
package k8s

import (
	"fmt"
	"os"
	"sync"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const localApiserverHost = "https://127.0.0.1:6443"

// KubeconfigAccessError is returned when there is an error reading the kubeconfig file.
type KubeconfigAccessError struct {
	Path string
	Err  error
}

func (e KubeconfigAccessError) Error() string {
	return fmt.Sprintf("reading kubeconfig %s: %v", e.Path, e.Err)
}

func (e KubeconfigAccessError) Unwrap() error {
	return e.Err
}

// ClientRegistry caches Kubernetes clients built from a kubeconfig file.
type ClientRegistry struct {
	mu             sync.Mutex
	kubeconfigPath string
	client         kubernetes.Interface
	dynamicClient  dynamic.Interface
	localClient    *kubernetes.Clientset
}

// NewClientRegistry creates a registry backed by kubeconfigPath.
func NewClientRegistry(kubeconfigPath string) *ClientRegistry {
	return &ClientRegistry{kubeconfigPath: kubeconfigPath}
}

// Client returns the cached clientset after the first successful build.
func (r *ClientRegistry) Client() (kubernetes.Interface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.client != nil {
		return r.client, nil
	}

	cfg, err := readKubeconfig(r.kubeconfigPath)
	if err != nil {
		return nil, err
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating k8s client: %w", err)
	}

	r.client = client

	return r.client, nil
}

// DynamicClient returns the cached dynamic client after the first successful build.
func (r *ClientRegistry) DynamicClient() (dynamic.Interface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dynamicClient != nil {
		return r.dynamicClient, nil
	}

	cfg, err := readKubeconfig(r.kubeconfigPath)
	if err != nil {
		return nil, err
	}

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic k8s client: %w", err)
	}

	r.dynamicClient = client

	return r.dynamicClient, nil
}

// LocalClient returns a cached clientset targeting this node's local apiserver.
// It uses the kubeconfig credentials but overrides the host to loopback so
// callers poll this apiserver instance instead of a cluster VIP. TLS
// verification is skipped because the apiserver certificate may not include
// 127.0.0.1 in its SANs.
func (r *ClientRegistry) LocalClient() (*kubernetes.Clientset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.localClient != nil {
		return r.localClient, nil
	}

	cfg, err := readKubeconfig(r.kubeconfigPath)
	if err != nil {
		return nil, err
	}
	cfg = localApiserverConfig(cfg)

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating local k8s client: %w", err)
	}

	r.localClient = client

	return r.localClient, nil
}

func readKubeconfig(path string) (*rest.Config, error) {
	kubeconfig, err := os.ReadFile(path)
	if err != nil {
		return nil, KubeconfigAccessError{Path: path, Err: err}
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}

	return cfg, nil
}

func localApiserverConfig(cfg *rest.Config) *rest.Config {
	localCfg := rest.CopyConfig(cfg)
	localCfg.Host = localApiserverHost
	localCfg.TLSClientConfig.Insecure = true
	localCfg.TLSClientConfig.CAData = nil
	localCfg.TLSClientConfig.CAFile = ""

	return localCfg
}
