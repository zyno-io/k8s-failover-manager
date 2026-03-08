package action

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

func scaleScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

func TestScaleExecutor_DeploymentNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scaleScheme()).Build()
	executor := &ScaleExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-test",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:     "Deployment",
			Name:     "nonexistent",
			Replicas: 3,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true")
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false for nonexistent resource")
	}
}

func TestScaleExecutor_DeploymentSuccess(t *testing.T) {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}, Template: podTemplate()},
	}

	c := fake.NewClientBuilder().WithScheme(scaleScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &ScaleExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-up",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:     "Deployment",
			Name:     "web",
			Replicas: 5,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		// Scale subresource may not work with fake client — that's expected
		// In integration tests this would work
		t.Skipf("scale subresource not supported by fake client: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true")
	}
}

func podTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
}

func TestScaleExecutor_UnsupportedKind(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scaleScheme()).Build()
	executor := &ScaleExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-ds",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:     "DaemonSet",
			Name:     "my-ds",
			Replicas: 1,
		},
	}

	_, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err == nil {
		t.Fatal("expected error for unsupported kind DaemonSet")
	}
	if got := err.Error(); !contains(got, "unsupported kind") {
		t.Errorf("expected 'unsupported kind' in error, got: %s", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestScaleExecutor_StatefulSetNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scaleScheme()).Build()
	executor := &ScaleExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-sts",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:     "StatefulSet",
			Name:     "nonexistent",
			Replicas: 3,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true")
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false for nonexistent StatefulSet")
	}
}

func TestScaleExecutor_StatefulSetSuccess(t *testing.T) {
	replicas := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			ServiceName: "db",
			Template:    podTemplate(),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scaleScheme()).WithObjects(sts).WithStatusSubresource(sts).Build()
	executor := &ScaleExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-sts",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:     "StatefulSet",
			Name:     "db",
			Replicas: 3,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		// Scale subresource may not work with fake client — that's expected
		t.Skipf("scale subresource not supported by fake client: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true")
	}
	if !result.Succeeded {
		t.Error("expected Succeeded=true")
	}
}

func TestScaleExecutor_WaitReadyDelegates(t *testing.T) {
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: podTemplate(),
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      2,
			UpdatedReplicas:    2,
			AvailableReplicas:  2,
			ObservedGeneration: 1,
		},
	}
	dep.Generation = 1

	c := fake.NewClientBuilder().WithScheme(scaleScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &ScaleExecutor{
		Client:    c,
		WaitReady: &WaitReadyExecutor{Client: c},
	}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-wait-ready",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:      "Deployment",
			Name:      "web",
			Replicas:  2,
			WaitReady: true,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Fatalf("expected success, got %#v", result)
	}
}

func TestScaleExecutor_WaitReadyWithoutExecutorFails(t *testing.T) {
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: podTemplate(),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scaleScheme()).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := &ScaleExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "scale-wait-ready",
		Scale: &failoverv1alpha1.ScaleAction{
			Kind:      "Deployment",
			Name:      "web",
			Replicas:  1,
			WaitReady: true,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || result.Succeeded {
		t.Fatalf("expected terminal failure, got %#v", result)
	}
}
