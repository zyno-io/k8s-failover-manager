package action

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

func waitReadyScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

func TestWaitReadyExecutor_DeploymentReady(t *testing.T) {
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: wrPodTemplate()},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      3,
			UpdatedReplicas:    3,
			AvailableReplicas:  3,
			ObservedGeneration: 2,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-web",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true for ready deployment")
	}
	if !result.Succeeded {
		t.Error("expected Succeeded=true")
	}
}

func TestWaitReadyExecutor_DeploymentNotReady(t *testing.T) {
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: wrPodTemplate()},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      1,
			ObservedGeneration: 2,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-web",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Error("expected Completed=false for not-ready deployment")
	}
}

func TestWaitReadyExecutor_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-missing",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "missing"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true for not-found")
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false for not-found")
	}
}

func TestCheckTimeout(t *testing.T) {
	// Not timed out
	now := metav1.Now()
	if CheckTimeout(&now, 300) {
		t.Error("expected not timed out for just-started action")
	}

	// Timed out
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	if !CheckTimeout(&past, 300) {
		t.Error("expected timed out for action started 10m ago with 300s timeout")
	}

	// Nil startedAt
	if CheckTimeout(nil, 300) {
		t.Error("expected not timed out for nil startedAt")
	}
}

func TestWaitReadyExecutor_StatefulSetReady(t *testing.T) {
	replicas := int32(2)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}, Template: wrPodTemplate(), ServiceName: "db"},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:      2,
			UpdatedReplicas:    2,
			AvailableReplicas:  2,
			ObservedGeneration: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(sts).WithStatusSubresource(sts).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-db",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "StatefulSet", Name: "db"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Errorf("expected ready statefulset: completed=%v, succeeded=%v", result.Completed, result.Succeeded)
	}
}

func wrPodTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
}

func TestWaitReadyExecutor_UnsupportedKind(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-pod",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Pod", Name: "my-pod"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true for unsupported kind")
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false for unsupported kind")
	}
	if result.Message != `unsupported kind "Pod"` {
		t.Errorf("unexpected message: %q", result.Message)
	}
}

func TestWaitReadyExecutor_NilReplicas(t *testing.T) {
	// Deployment with nil Spec.Replicas defaults to 1
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: wrPodTemplate()},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ObservedGeneration: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-nil-replicas",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Errorf("expected ready with nil replicas (defaults to 1): completed=%v, succeeded=%v", result.Completed, result.Succeeded)
	}
}

func TestWaitReadyExecutor_ZeroReplicas(t *testing.T) {
	replicas := int32(0)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: wrPodTemplate()},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      0,
			UpdatedReplicas:    0,
			AvailableReplicas:  0,
			ObservedGeneration: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-zero",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Errorf("expected ready with 0 desired/0 ready: completed=%v, succeeded=%v", result.Completed, result.Succeeded)
	}
}

func TestWaitReadyExecutor_GenerationMismatch(t *testing.T) {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 5},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: wrPodTemplate()},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ObservedGeneration: 4, // doesn't match Generation=5
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-gen-mismatch",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Error("expected Completed=false when generation != observedGeneration")
	}
}

func TestWaitReadyExecutor_StatefulSetNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-sts-missing",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "StatefulSet", Name: "missing"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true for not-found StatefulSet")
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false for not-found StatefulSet")
	}
}

func TestWaitReadyExecutor_StatefulSetNotReady(t *testing.T) {
	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}, Template: wrPodTemplate(), ServiceName: "db"},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:      1,
			UpdatedReplicas:    2,
			AvailableReplicas:  1,
			ObservedGeneration: 2,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(sts).WithStatusSubresource(sts).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-sts-not-ready",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "StatefulSet", Name: "db"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Error("expected Completed=false for not-ready StatefulSet")
	}
}

func TestWaitReadyExecutor_StatefulSetNilReplicas(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}, Template: wrPodTemplate(), ServiceName: "db"},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:      1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ObservedGeneration: 1,
		},
	}

	c := fake.NewClientBuilder().WithScheme(waitReadyScheme()).WithObjects(sts).WithStatusSubresource(sts).Build()
	executor := &WaitReadyExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name:      "wait-sts-nil",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "StatefulSet", Name: "db"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Errorf("expected ready with nil replicas (defaults to 1): completed=%v, succeeded=%v", result.Completed, result.Succeeded)
	}
}
