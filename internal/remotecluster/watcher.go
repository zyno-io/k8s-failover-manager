package remotecluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	toolswatch "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	listTimeout = 15 * time.Second
	// minHealthyDuration is how long a watch session must run before we consider
	// it healthy and reset the reconnect backoff.
	minHealthyDuration = 30 * time.Second
	// configPollInterval is how often we check for kubeconfig Secret changes
	// during an active watch session.
	configPollInterval = 30 * time.Second
)

var failoverServiceGVR = schema.GroupVersionResource{
	Group:    "k8s-failover.zyno.io",
	Version:  "v1alpha1",
	Resource: "failoverservices",
}

// RemoteWatcher watches FailoverService resources on the remote cluster and
// enqueues reconcile requests when changes are detected. It handles reconnection
// automatically with exponential backoff and plugs into the controller as a
// source.TypedSource[reconcile.Request].
type RemoteWatcher struct {
	ClientProvider *ClientProvider

	mu            sync.Mutex
	lastConfigVer string
	listClient    dynamic.Interface
	watchClient   dynamic.Interface
	queue         workqueue.TypedRateLimitingInterface[reconcile.Request]
}

// Start is called by the controller framework to register the workqueue.
// It launches the watch loop in a background goroutine.
func (w *RemoteWatcher) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	w.queue = queue
	go w.run(ctx)
	return nil
}

// run is the main loop that establishes and maintains the remote watch.
func (w *RemoteWatcher) run(ctx context.Context) {
	log.Info("Remote watcher starting")
	defer log.Info("Remote watcher stopped")

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		start := time.Now()
		err := w.watchLoop(ctx)
		if ctx.Err() != nil {
			return
		}

		// Reset backoff if the watch ran long enough to be considered healthy.
		// This avoids staying at maxBackoff after a transient failure followed
		// by a long healthy session.
		if time.Since(start) >= minHealthyDuration {
			backoff = time.Second
		}

		if err != nil {
			log.Error(err, "Remote watch error, will reconnect", "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// watchLoop establishes a single watch session. It lists first to get the
// current resource version and enqueue all existing items (so changes during
// a blind period trigger immediate reconciles), then starts a RetryWatcher
// from that point. RetryWatcher handles API timeouts and watch restarts internally.
//
// The loop also periodically checks whether the kubeconfig Secret has changed
// and tears down the watch so the next iteration uses fresh credentials.
func (w *RemoteWatcher) watchLoop(ctx context.Context) error {
	listDynClient, watchDynClient, configVer, err := w.getDynamicClients(ctx)
	if err != nil {
		return fmt.Errorf("getting dynamic clients: %w", err)
	}

	// Use a bounded context for the initial List so it can't hang indefinitely
	// on a flaky network.
	listCtx, listCancel := context.WithTimeout(ctx, listTimeout)
	defer listCancel()

	list, err := listDynClient.Resource(failoverServiceGVR).
		Namespace("").
		List(listCtx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing remote FailoverServices: %w", err)
	}
	rv := list.GetResourceVersion()

	// Enqueue all items from the list so that any changes that occurred while
	// the watch was disconnected trigger immediate reconciles.
	w.enqueueList(list)

	// Create a cancellable context for this watch session so we can tear it
	// down when kubeconfig changes.
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	watcher, err := toolswatch.NewRetryWatcherWithContext(watchCtx, rv, &watcherFunc{
		dynClient: watchDynClient,
	})
	if err != nil {
		return fmt.Errorf("creating retry watcher: %w", err)
	}
	defer watcher.Stop()

	log.Info("Remote watch established", "resourceVersion", rv, "existingItems", len(list.Items))

	// Periodically check if the kubeconfig Secret has changed. If it has,
	// cancel the watch so the next iteration picks up fresh credentials.
	configTicker := time.NewTicker(configPollInterval)
	defer configTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-configTicker.C:
			if w.configChanged(ctx, configVer) {
				log.Info("Kubeconfig Secret changed, restarting watch")
				return fmt.Errorf("kubeconfig rotated")
			}
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			if event.Type == watch.Error {
				return fmt.Errorf("watch error event received")
			}
			w.enqueueEvent(event)
		}
	}
}

// configChanged checks whether the kubeconfig Secret has been updated since
// the dynamic clients were created. It compares the Secret's resourceVersion
// directly rather than going through GetRESTConfig, so it detects changes even
// when the new Secret content is invalid or the Secret has been deleted.
func (w *RemoteWatcher) configChanged(ctx context.Context, knownVer string) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	currentVer, err := w.ClientProvider.SecretResourceVersion(checkCtx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Secret was deleted — credentials are definitely stale.
			log.Error(err, "Kubeconfig Secret deleted, forcing watch restart")
			return true
		}
		// Transient error (timeout, network blip, RBAC flap) — don't tear
		// down a healthy watch. We'll check again next poll interval.
		log.V(1).Info("Transient error checking kubeconfig Secret, keeping watch alive", "error", err)
		return false
	}
	return currentVer != knownVer
}

// enqueueList enqueues a reconcile request for every item in a list result.
func (w *RemoteWatcher) enqueueList(list *unstructured.UnstructuredList) {
	for i := range list.Items {
		item := &list.Items[i]
		w.queue.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      item.GetName(),
				Namespace: item.GetNamespace(),
			},
		})
	}
}

// enqueueEvent extracts the name/namespace from a watch event and adds a reconcile
// request to the controller's workqueue.
func (w *RemoteWatcher) enqueueEvent(event watch.Event) {
	obj, ok := event.Object.(metav1.Object)
	if !ok {
		return
	}
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
		},
	}
	w.queue.Add(req)
	log.V(1).Info("Remote change detected, enqueued reconcile",
		"name", obj.GetName(), "namespace", obj.GetNamespace(), "type", event.Type)
}

// getDynamicClients returns two dynamic clients (list and watch) plus the
// config version they were built from. Both are refreshed atomically when
// the underlying kubeconfig Secret changes.
func (w *RemoteWatcher) getDynamicClients(ctx context.Context) (listClient, watchClient dynamic.Interface, configVer string, err error) {
	cfg, configVer, err := w.ClientProvider.GetRESTConfig(ctx)
	if err != nil {
		return nil, nil, "", err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.listClient != nil && w.lastConfigVer == configVer {
		return w.listClient, w.watchClient, configVer, nil
	}

	// List client: keeps the tuned timeout from GetRESTConfig.
	lc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("creating list dynamic client: %w", err)
	}

	// Watch client: no request timeout since watches are long-lived.
	watchCfg := rest.CopyConfig(cfg)
	watchCfg.Timeout = 0
	wc, err := dynamic.NewForConfig(watchCfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("creating watch dynamic client: %w", err)
	}

	w.listClient = lc
	w.watchClient = wc
	w.lastConfigVer = configVer
	log.Info("Remote dynamic clients created/refreshed for watcher")
	return lc, wc, configVer, nil
}

// watcherFunc implements cache.WatcherWithContext for use with RetryWatcher.
type watcherFunc struct {
	dynClient dynamic.Interface
}

func (wf *watcherFunc) WatchWithContext(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return wf.dynClient.Resource(failoverServiceGVR).
		Namespace("").
		Watch(ctx, opts)
}

// compile-time interface checks
var (
	_ cache.WatcherWithContext              = (*watcherFunc)(nil)
	_ source.TypedSource[reconcile.Request] = (*RemoteWatcher)(nil)
)
