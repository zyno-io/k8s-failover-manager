package remotecluster

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("remote-cluster")

// ClientProvider manages a cached client for the remote cluster.
type ClientProvider struct {
	LocalClient     client.Client
	Scheme          *runtime.Scheme
	SecretName      string
	SecretNamespace string

	mu              sync.Mutex
	cachedClient    client.Client
	cachedConfig    *rest.Config
	lastResourceVer string
}

// GetClient returns a client for the remote cluster, reading the kubeconfig from a Secret.
// The client is cached and invalidated if the Secret's resourceVersion changes.
func (p *ClientProvider) GetClient(ctx context.Context) (client.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	secret := &corev1.Secret{}
	if err := p.LocalClient.Get(ctx, types.NamespacedName{
		Name:      p.SecretName,
		Namespace: p.SecretNamespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("getting remote kubeconfig secret: %w", err)
	}

	if p.cachedClient != nil && p.lastResourceVer == secret.ResourceVersion {
		return p.cachedClient, nil
	}

	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s missing 'kubeconfig' key", p.SecretNamespace, p.SecretName)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("parsing remote kubeconfig: %w", err)
	}

	tuneRESTConfig(restConfig)

	c, err := client.New(restConfig, client.Options{Scheme: p.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating remote client: %w", err)
	}

	p.cachedClient = c
	p.cachedConfig = restConfig
	p.lastResourceVer = secret.ResourceVersion
	log.Info("Remote cluster client created/refreshed", "secret", p.SecretName, "namespace", p.SecretNamespace)

	return c, nil
}

// GetRESTConfig returns a REST config for the remote cluster and the Secret
// resourceVersion it was derived from. Both are returned atomically so callers
// can cache based on the version without TOCTOU races. The config is cached
// alongside the client and invalidated when the kubeconfig Secret changes.
func (p *ClientProvider) GetRESTConfig(ctx context.Context) (*rest.Config, string, error) {
	// GetClient handles caching and refresh; call it to ensure cachedConfig is current.
	if _, err := p.GetClient(ctx); err != nil {
		return nil, "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return rest.CopyConfig(p.cachedConfig), p.lastResourceVer, nil
}

// SecretResourceVersion returns the current resourceVersion of the kubeconfig Secret.
// This is a lightweight check that does not parse the kubeconfig or create a client,
// making it suitable for polling whether credentials have changed even when the new
// Secret content is invalid.
func (p *ClientProvider) SecretResourceVersion(ctx context.Context) (string, error) {
	secret := &corev1.Secret{}
	if err := p.LocalClient.Get(ctx, types.NamespacedName{
		Name:      p.SecretName,
		Namespace: p.SecretNamespace,
	}, secret); err != nil {
		return "", fmt.Errorf("getting remote kubeconfig secret: %w", err)
	}
	return secret.ResourceVersion, nil
}

// tuneRESTConfig applies transport settings optimized for cross-cluster communication
// over potentially unreliable networks.
func tuneRESTConfig(cfg *rest.Config) {
	cfg.Dial = (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 10 * time.Second,
	}).DialContext
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
}
