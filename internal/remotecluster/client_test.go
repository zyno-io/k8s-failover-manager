package remotecluster

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetClient_CachesClient(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Create a minimal kubeconfig that can be parsed (even if not connectable)
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://localhost:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: fake-token
`

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "remote-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte(kubeconfig),
		},
	}

	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}

	// First call should create client
	client1, err := provider.GetClient(ctx)
	if err != nil {
		t.Fatalf("GetClient error: %v", err)
	}
	if client1 == nil {
		t.Fatal("expected non-nil client")
	}

	// Second call should return cached client
	client2, err := provider.GetClient(ctx)
	if err != nil {
		t.Fatalf("GetClient second call error: %v", err)
	}

	// Should be the same instance (pointer equality)
	if client1 != client2 {
		t.Error("expected same client instance from cache")
	}
	if provider.lastResourceVer != "1" {
		t.Errorf("expected lastResourceVer=1, got %s", provider.lastResourceVer)
	}
}

func TestGetClient_MissingKubeconfigKey(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "remote-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"wrong-key": []byte("data"),
		},
	}

	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}

	_, err := provider.GetClient(ctx)
	if err == nil {
		t.Fatal("expected error for missing kubeconfig key")
	}
}

func TestGetClient_SecretNotFound(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	localClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "nonexistent",
		SecretNamespace: "test-ns",
	}

	_, err := provider.GetClient(ctx)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestGetClient_InvalidKubeconfig(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "remote-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte("this is not valid kubeconfig data!!!"),
		},
	}

	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}

	_, err := provider.GetClient(ctx)
	if err == nil {
		t.Fatal("expected error for garbage kubeconfig data")
	}
}

func TestGetClient_RefreshesOnVersionChange(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://localhost:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: fake-token
`

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "remote-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte(kubeconfig),
		},
	}

	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}

	// First call — creates client
	client1, err := provider.GetClient(ctx)
	if err != nil {
		t.Fatalf("GetClient error: %v", err)
	}

	// Re-fetch the secret to get the current ResourceVersion (fake client may have changed it)
	freshSecret := &corev1.Secret{}
	if err := localClient.Get(ctx, types.NamespacedName{Name: "remote-kubeconfig", Namespace: "test-ns"}, freshSecret); err != nil {
		t.Fatalf("re-fetch secret: %v", err)
	}
	freshSecret.Data["kubeconfig"] = []byte(kubeconfig)
	if err := localClient.Update(ctx, freshSecret); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	// Second call — should detect version change and create a new client
	client2, err := provider.GetClient(ctx)
	if err != nil {
		t.Fatalf("GetClient after version change error: %v", err)
	}

	if client1 == client2 {
		t.Error("expected different client instance after ResourceVersion change")
	}
}
