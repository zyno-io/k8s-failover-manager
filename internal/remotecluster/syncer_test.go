package remotecluster

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = failoverv1alpha1.AddToScheme(s)
	return s
}

func newFS(failoverActive bool, activeGen int64, activeCluster, clusterID string) *failoverv1alpha1.FailoverService {
	fs := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: failoverv1alpha1.FailoverServiceSpec{
			ServiceName:    "test-svc",
			FailoverActive: failoverActive,
			Ports:          []failoverv1alpha1.Port{{Name: "tcp", Port: 3306, Protocol: "TCP"}},
			PrimaryCluster: failoverv1alpha1.ClusterTarget{
				PrimaryModeAddress:  "primary.example.com",
				FailoverModeAddress: "10.0.0.1",
			},
			FailoverCluster: failoverv1alpha1.ClusterTarget{
				PrimaryModeAddress:  "10.0.0.2",
				FailoverModeAddress: "failover.example.com",
			},
		},
		Status: failoverv1alpha1.FailoverServiceStatus{
			ActiveGeneration: activeGen,
			ActiveCluster:    activeCluster,
		},
	}
	if clusterID != "" {
		SetClusterIDAnnotation(fs, clusterID)
	}
	return fs
}

// newTestSyncer creates a Syncer with a fake local client (for the kubeconfig secret)
// and a pre-supplied remote client. This bypasses the real kubeconfig parsing path
// by pre-populating the cached client.
func newTestSyncer(scheme *runtime.Scheme, remoteClient client.Client, localClusterID string) *Syncer {
	// Create a dummy secret so GetClient doesn't panic on LocalClient.Get
	dummySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-kubeconfig",
			Namespace:       "test-ns",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{"kubeconfig": []byte("unused")},
	}
	localClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dummySecret).Build()

	return &Syncer{
		ClientProvider: &ClientProvider{
			LocalClient:     localClient,
			Scheme:          scheme,
			SecretName:      "test-kubeconfig",
			SecretNamespace: "test-ns",
			cachedClient:    remoteClient,
			lastResourceVer: "1", // matches the secret's resourceVersion so cache is used
		},
		ClusterID: localClusterID,
	}
}

func TestSync_NoConflict(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	local := newFS(false, 1, "local", "local")
	remote := newFS(false, 1, "remote", "remote")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "local")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if result.RemoteWins {
		t.Error("expected local to win when both agree")
	}
	if result.RemoteState == nil {
		t.Fatal("expected RemoteState to be set")
	}
	if result.RemoteState.FailoverActive != false {
		t.Error("expected RemoteState.FailoverActive=false")
	}
}

func TestSync_RemoteWins_HigherGeneration(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	local := newFS(false, 1, "local", "local")
	remote := newFS(true, 5, "remote", "remote")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "local")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if !result.RemoteWins {
		t.Error("expected remote to win with higher generation")
	}
}

func TestSync_RemoteWins_NewerSpecChangeTime(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	local := newFS(false, 3, "local", "local")
	remote := newFS(true, 3, "remote", "remote")
	localSpecChangeTime := metav1.NewTime(time.Unix(100, 0))
	remoteSpecChangeTime := metav1.NewTime(time.Unix(200, 0))
	local.Status.SpecChangeTime = &localSpecChangeTime
	remote.Status.SpecChangeTime = &remoteSpecChangeTime

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "local")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if !result.RemoteWins {
		t.Fatal("expected remote to win with newer specChangeTime")
	}
	if result.RemoteState == nil || result.RemoteState.SpecChangeTime == nil {
		t.Fatalf("expected RemoteState.SpecChangeTime, got %#v", result.RemoteState)
	}
}

func TestSync_RemoteWins_LexicographicTiebreak(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Same generation, remote cluster ID "z-cluster" > local "a-cluster"
	local := newFS(false, 3, "a-cluster", "a-cluster")
	remote := newFS(true, 3, "z-cluster", "z-cluster")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "a-cluster")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if !result.RemoteWins {
		t.Error("expected remote to win lexicographic tiebreak (z-cluster > a-cluster)")
	}
}

func TestSync_LocalWins_Push(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Local has higher generation
	local := newFS(true, 5, "local", "local")
	remote := newFS(false, 1, "remote", "remote")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "local")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if result.RemoteWins {
		t.Error("expected local to win with higher generation")
	}
	if result.RemoteState == nil || result.RemoteState.FailoverActive != true {
		t.Error("expected RemoteState.FailoverActive=true after successful local push")
	}

	// Verify remote was updated
	updatedRemote := &failoverv1alpha1.FailoverService{}
	if err := remoteClient.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, updatedRemote); err != nil {
		t.Fatalf("Get remote error: %v", err)
	}
	if !updatedRemote.Spec.FailoverActive {
		t.Error("expected remote failoverActive to be updated to true")
	}
}

func TestSync_LocalWins_LexicographicTiebreak(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Same generation, local cluster ID "z-cluster" > remote "a-cluster"
	local := newFS(true, 3, "z-cluster", "z-cluster")
	remote := newFS(false, 3, "a-cluster", "a-cluster")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "z-cluster")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if result.RemoteWins {
		t.Error("expected local to win lexicographic tiebreak (z-cluster > a-cluster)")
	}
	if result.RemoteState == nil || result.RemoteState.FailoverActive != true {
		t.Error("expected RemoteState.FailoverActive=true after successful local push")
	}

	// Verify remote was updated
	updatedRemote := &failoverv1alpha1.FailoverService{}
	if err := remoteClient.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, updatedRemote); err != nil {
		t.Fatalf("Get remote error: %v", err)
	}
	if !updatedRemote.Spec.FailoverActive {
		t.Error("expected remote failoverActive to be updated to true")
	}
}

func TestSync_LocalWins_NewerSpecChangeTime(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	local := newFS(true, 3, "local", "local")
	remote := newFS(false, 3, "remote", "remote")
	localSpecChangeTime := metav1.NewTime(time.Unix(200, 0))
	remoteSpecChangeTime := metav1.NewTime(time.Unix(100, 0))
	local.Status.SpecChangeTime = &localSpecChangeTime
	remote.Status.SpecChangeTime = &remoteSpecChangeTime

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "local")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if result.RemoteWins {
		t.Fatal("expected local to win with newer specChangeTime")
	}

	updatedRemote := &failoverv1alpha1.FailoverService{}
	if err := remoteClient.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, updatedRemote); err != nil {
		t.Fatalf("Get remote error: %v", err)
	}
	if !updatedRemote.Spec.FailoverActive {
		t.Fatal("expected remote failoverActive to be updated to true")
	}
}

func TestSync_GenerationTie_LexicographicWins(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Equal generation, remote ClusterID "m-cluster" > local "a-cluster" → remote wins
	local := newFS(false, 3, "a-cluster", "a-cluster")
	remote := newFS(true, 3, "m-cluster", "m-cluster")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "a-cluster")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	if !result.RemoteWins {
		t.Error("expected remote to win: equal generation and remote ID > local ID")
	}
}

func TestSync_RemoteNotFound(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	local := newFS(false, 1, "local", "local")
	// Remote client with NO FailoverService object
	remoteClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	syncer := newTestSyncer(scheme, remoteClient, "local")

	_, err := syncer.Sync(ctx, local)
	if err == nil {
		t.Fatal("expected error when remote FailoverService does not exist")
	}
}

func TestSyncTimeout_Default(t *testing.T) {
	syncer := &Syncer{}
	if got := syncer.syncTimeout(); got != DefaultSyncTimeout {
		t.Errorf("expected DefaultSyncTimeout (%v), got %v", DefaultSyncTimeout, got)
	}
}

func TestSyncTimeout_Custom(t *testing.T) {
	syncer := &Syncer{SyncTimeout: 10 * time.Second}
	if got := syncer.syncTimeout(); got != 10*time.Second {
		t.Errorf("expected 10s, got %v", got)
	}
}

func TestSync_EmptyClusterIDs(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Both cluster IDs empty, same generation, conflicting failoverActive
	local := newFS(false, 3, "", "")
	remote := newFS(true, 3, "", "")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	// Neither "" > "" is true, so local wins
	if result.RemoteWins {
		t.Error("expected local to win when both cluster IDs are empty")
	}
}

func TestSync_EqualClusterIDs(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Same non-empty cluster ID, same generation, conflicting failoverActive
	local := newFS(false, 3, "same-cluster", "same-cluster")
	remote := newFS(true, 3, "same-cluster", "same-cluster")

	remoteClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remote).WithStatusSubresource(remote).Build()
	syncer := newTestSyncer(scheme, remoteClient, "same-cluster")

	result, err := syncer.Sync(ctx, local)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	// "same-cluster" > "same-cluster" is false, so local wins
	if result.RemoteWins {
		t.Error("expected local to win when both cluster IDs are identical")
	}
}
