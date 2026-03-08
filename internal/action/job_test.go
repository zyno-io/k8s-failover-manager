package action

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

func jobScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func TestJobExecutor_Create(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	executor := &JobExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-job",
		Job: &failoverv1alpha1.JobAction{
			Image:   "alpine:latest",
			Command: []string{"echo", "hello"},
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Error("expected Completed=false for newly created job")
	}
	if result.JobName != "test-fs-action-0" {
		t.Errorf("expected job name test-fs-action-0, got %s", result.JobName)
	}

	// Verify job was created
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-fs-action-0", Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}
	if job.Spec.Template.Spec.Containers[0].Image != "alpine:latest" {
		t.Errorf("unexpected image: %s", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestJobExecutor_Running(t *testing.T) {
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-fs-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "action", Image: "alpine"}}}}},
	}

	c := fake.NewClientBuilder().WithScheme(jobScheme()).WithObjects(existingJob).Build()
	executor := &JobExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-job",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Error("expected Completed=false for running job")
	}
}

func TestJobExecutor_Succeeded(t *testing.T) {
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-fs-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "action", Image: "alpine"}}}}},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(jobScheme()).WithObjects(existingJob).Build()
	executor := &JobExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-job",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true for succeeded job")
	}
	if !result.Succeeded {
		t.Error("expected Succeeded=true")
	}
}

func TestJobExecutor_Failed(t *testing.T) {
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-fs-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "action", Image: "alpine"}}}}},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(jobScheme()).WithObjects(existingJob).Build()
	executor := &JobExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-job",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true for failed job")
	}
	if result.Succeeded {
		t.Error("expected Succeeded=false")
	}
}

func TestJobExecutor_Script(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	executor := &JobExecutor{Client: c}

	script := "echo hello && sleep 1"
	action := &failoverv1alpha1.FailoverAction{
		Name: "script-job",
		Job: &failoverv1alpha1.JobAction{
			Image:  "alpine:latest",
			Script: &script,
		},
	}

	_, err := executor.Execute(context.Background(), action, "default", "test-fs", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-fs-action-1", Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}

	cmd := job.Spec.Template.Spec.Containers[0].Command
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != script {
		t.Errorf("expected script command, got %v", cmd)
	}
}

func TestJobExecutor_ServiceAccountAndRetries(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	executor := &JobExecutor{Client: c}

	retries := int32(3)
	deadline := int64(120)
	action := &failoverv1alpha1.FailoverAction{
		Name: "test-job",
		Job: &failoverv1alpha1.JobAction{
			Image:                 "alpine",
			ServiceAccountName:    "my-sa",
			MaxRetries:            &retries,
			ActiveDeadlineSeconds: &deadline,
		},
	}

	_, err := executor.Execute(context.Background(), action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-fs-action-0", Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}

	if job.Spec.Template.Spec.ServiceAccountName != "my-sa" {
		t.Errorf("expected serviceAccountName my-sa, got %s", job.Spec.Template.Spec.ServiceAccountName)
	}
	if *job.Spec.BackoffLimit != 3 {
		t.Errorf("expected backoffLimit 3, got %d", *job.Spec.BackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != 120 {
		t.Errorf("expected activeDeadlineSeconds 120, got %d", *job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobExecutor_Cleanup(t *testing.T) {
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-fs-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "action", Image: "alpine"}}}}},
	}

	c := fake.NewClientBuilder().WithScheme(jobScheme()).WithObjects(existingJob).Build()
	executor := &JobExecutor{Client: c}

	err := executor.Cleanup(context.Background(), "test-fs-action-0", "default")
	if err != nil {
		t.Fatalf("cleanup error: %v", err)
	}

	// Verify job is deleted
	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-fs-action-0", Namespace: "default"}, job)
	if err == nil {
		t.Error("expected job to be deleted")
	}
}

func TestJobExecutor_Cleanup_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	executor := &JobExecutor{Client: c}

	err := executor.Cleanup(context.Background(), "nonexistent", "default")
	if err != nil {
		t.Fatalf("expected no error for missing job, got: %v", err)
	}
}

func TestJobName_Short(t *testing.T) {
	name := jobName("test-fs", 0)
	if name != "test-fs-action-0" {
		t.Errorf("expected test-fs-action-0, got %s", name)
	}
}

func TestJobName_ExactLimit(t *testing.T) {
	// "x" * 54 + "-action-0" = 63 chars exactly
	prefix := strings.Repeat("x", 54)
	name := jobName(prefix, 0)
	if len(name) != 63 {
		t.Errorf("expected length 63, got %d", len(name))
	}
	if name != prefix+"-action-0" {
		t.Error("expected no truncation at exactly 63 chars")
	}
}

func TestJobName_Truncated(t *testing.T) {
	// Create a prefix that will exceed 63 chars
	prefix := strings.Repeat("a", 60)
	name := jobName(prefix, 0)
	if len(name) > maxJobNameLen {
		t.Errorf("name exceeds max length: %d > %d", len(name), maxJobNameLen)
	}
	if len(name) != maxJobNameLen {
		t.Errorf("expected exactly %d chars, got %d", maxJobNameLen, len(name))
	}
}

func TestJobName_DeterministicHash(t *testing.T) {
	prefix := strings.Repeat("b", 60)
	name1 := jobName(prefix, 5)
	name2 := jobName(prefix, 5)
	if name1 != name2 {
		t.Errorf("expected deterministic name, got %q and %q", name1, name2)
	}
	// Different index should produce a different name
	name3 := jobName(prefix, 6)
	if name1 == name3 {
		t.Error("expected different names for different action indices")
	}
}

func TestJobExecutor_EnvVars(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	executor := &JobExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "env-job",
		Job: &failoverv1alpha1.JobAction{
			Image: "alpine",
			Env: []failoverv1alpha1.EnvVar{
				{Name: "FOO", Value: "bar"},
				{Name: "BAZ", Value: "qux"},
			},
		},
	}

	_, err := executor.Execute(context.Background(), action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-fs-action-0", Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	if len(envVars) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(envVars))
	}
	if envVars[0].Name != "FOO" || envVars[0].Value != "bar" {
		t.Errorf("unexpected env var: %+v", envVars[0])
	}
	if envVars[1].Name != "BAZ" || envVars[1].Value != "qux" {
		t.Errorf("unexpected env var: %+v", envVars[1])
	}
}

func TestJobExecutor_OwnerRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	ownerRef := &metav1.OwnerReference{
		APIVersion: "k8s-failover.zyno.io/v1alpha1",
		Kind:       "FailoverService",
		Name:       "test-fs",
		UID:        "test-uid",
	}
	executor := &JobExecutor{Client: c}

	action := &failoverv1alpha1.FailoverAction{
		Name: "owner-job",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	ctx := ContextWithOwnerRef(context.Background(), ownerRef)
	_, err := executor.Execute(ctx, action, "default", "test-fs", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-fs-action-0", Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}

	if len(job.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(job.OwnerReferences))
	}
	if job.OwnerReferences[0].Name != "test-fs" {
		t.Errorf("expected owner name test-fs, got %s", job.OwnerReferences[0].Name)
	}
}

func TestJobExecutor_LongOwnerLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(jobScheme()).Build()
	executor := &JobExecutor{Client: c}

	longPrefix := strings.Repeat("z", 80)
	action := &failoverv1alpha1.FailoverAction{
		Name: "label-job",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	_, err := executor.Execute(context.Background(), action, "default", longPrefix, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	job := &batchv1.Job{}
	jobN := jobName(longPrefix, 0)
	err = c.Get(context.Background(), types.NamespacedName{Name: jobN, Namespace: "default"}, job)
	if err != nil {
		t.Fatalf("job not found: %v", err)
	}

	ownerLabel := job.Labels["k8s-failover.zyno.io/owner"]
	if len(ownerLabel) > 63 {
		t.Errorf("owner label exceeds 63 chars: %d", len(ownerLabel))
	}
}

func TestTruncateLabel(t *testing.T) {
	short := "my-service"
	if got := truncateLabel(short); got != short {
		t.Errorf("expected no truncation, got %q", got)
	}

	exact := strings.Repeat("x", 63)
	if got := truncateLabel(exact); got != exact {
		t.Errorf("expected no truncation at 63 chars, got len %d", len(got))
	}

	long := strings.Repeat("a", 80)
	got := truncateLabel(long)
	if len(got) > 63 {
		t.Errorf("expected truncation to 63, got %d", len(got))
	}
	// Deterministic
	if got2 := truncateLabel(long); got != got2 {
		t.Error("expected deterministic output")
	}
}
