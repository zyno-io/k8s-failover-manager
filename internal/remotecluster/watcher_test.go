package remotecluster

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestRemoteWatcher_Enqueue(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
	defer queue.ShutDown()

	w := &RemoteWatcher{}
	w.queue = queue

	obj := &unstructured.Unstructured{}
	obj.SetName("test-svc")
	obj.SetNamespace("default")

	w.enqueueEvent(watch.Event{
		Type:   watch.Modified,
		Object: obj,
	})

	if queue.Len() != 1 {
		t.Fatalf("expected 1 item in queue, got %d", queue.Len())
	}

	item, shutdown := queue.Get()
	if shutdown {
		t.Fatal("queue shut down unexpectedly")
	}
	defer queue.Done(item)

	expected := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-svc",
			Namespace: "default",
		},
	}
	if item != expected {
		t.Errorf("expected %v, got %v", expected, item)
	}
}

func TestRemoteWatcher_StartReturnsNonBlocking(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
	defer queue.ShutDown()

	// Create a watcher with a client provider that will fail (secret doesn't exist).
	// We just want to verify Start() returns immediately (non-blocking).
	localClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          newScheme(),
		SecretName:      "nonexistent",
		SecretNamespace: "test-ns",
	}

	w := &RemoteWatcher{
		ClientProvider: provider,
	}

	ctx, cancel := context.WithCancel(context.Background())

	startReturned := make(chan error, 1)
	go func() {
		startReturned <- w.Start(ctx, queue)
	}()

	// Start() should return immediately since it launches the watch loop
	// in a goroutine. Give it a moment.
	time.Sleep(50 * time.Millisecond)

	// Cancel context to stop the background goroutine.
	cancel()

	select {
	case err := <-startReturned:
		if err != nil {
			t.Errorf("Start returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestRemoteWatcher_EnqueueList(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
	defer queue.ShutDown()

	w := &RemoteWatcher{}
	w.queue = queue

	list := &unstructured.UnstructuredList{}
	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		obj := unstructured.Unstructured{}
		obj.SetName(name)
		obj.SetNamespace("default")
		list.Items = append(list.Items, obj)
	}

	w.enqueueList(list)

	// The workqueue deduplicates by key, but all three have different names.
	if queue.Len() != 3 {
		t.Fatalf("expected 3 items in queue, got %d", queue.Len())
	}
}

func TestRemoteWatcher_ConfigChanged_SameVersion(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "remote-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "42",
		},
		Data: map[string][]byte{"kubeconfig": []byte("data")},
	}
	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}
	w := &RemoteWatcher{ClientProvider: provider}

	if w.configChanged(ctx, "42") {
		t.Error("expected configChanged=false for same version")
	}
}

func TestRemoteWatcher_ConfigChanged_DifferentVersion(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "remote-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "99",
		},
		Data: map[string][]byte{"kubeconfig": []byte("data")},
	}
	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}
	w := &RemoteWatcher{ClientProvider: provider}

	if !w.configChanged(ctx, "42") {
		t.Error("expected configChanged=true for different version")
	}
}

func TestRemoteWatcher_ConfigChanged_SecretDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// No secret exists — simulates deletion.
	localClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	provider := &ClientProvider{
		LocalClient:     localClient,
		Scheme:          scheme,
		SecretName:      "remote-kubeconfig",
		SecretNamespace: "test-ns",
	}
	w := &RemoteWatcher{ClientProvider: provider}

	if !w.configChanged(ctx, "42") {
		t.Error("expected configChanged=true when secret is deleted")
	}
}

func TestRemoteWatcher_EnqueueIgnoresNonObject(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
	defer queue.ShutDown()

	w := &RemoteWatcher{}
	w.queue = queue

	// Pass a status object — has no ObjectMeta.
	w.enqueueEvent(watch.Event{
		Type:   watch.Modified,
		Object: &metav1.Status{},
	})

	if queue.Len() != 0 {
		t.Fatalf("expected 0 items in queue for non-object event, got %d", queue.Len())
	}
}
