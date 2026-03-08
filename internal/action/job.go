package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

const maxJobNameLen = 63
const maxLabelValueLen = 63

const (
	defaultActiveDeadlineSeconds int64 = 300
	defaultBackoffLimit          int32 = 0
)

// JobExecutor executes Job-based actions.
type JobExecutor struct {
	Client client.Client
}

// jobName generates a deterministic job name from the prefix and index.
// Enforces the 63-character DNS label limit by truncating and appending a hash suffix.
func jobName(namePrefix string, actionIndex int) string {
	name := fmt.Sprintf("%s-action-%d", namePrefix, actionIndex)
	if len(name) <= maxJobNameLen {
		return name
	}
	// Truncate and append a short hash for uniqueness
	h := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(h[:4]) // 8 hex chars
	// Leave room for "-" + 8 char hash = 9 chars
	truncated := name[:maxJobNameLen-9]
	return truncated + "-" + suffix
}

// truncateLabel truncates a label value to the Kubernetes 63-character limit.
func truncateLabel(v string) string {
	if len(v) <= maxLabelValueLen {
		return v
	}
	h := sha256.Sum256([]byte(v))
	suffix := hex.EncodeToString(h[:4])
	return v[:maxLabelValueLen-9] + "-" + suffix
}

func (e *JobExecutor) Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error) {
	name := jobName(namePrefix, actionIndex)
	spec := action.Job

	// Check if Job already exists (idempotent)
	existing := &batchv1.Job{}
	err := e.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("checking job %s: %w", name, err)
	}

	if errors.IsNotFound(err) {
		// Create the Job
		job := e.buildJob(name, namespace, spec, namePrefix, OwnerRefFromContext(ctx))
		if err := e.Client.Create(ctx, job); err != nil {
			return nil, fmt.Errorf("creating job %s: %w", name, err)
		}
		return &Result{Completed: false, JobName: name, Message: "Job created"}, nil
	}

	// Job exists — check status
	return e.checkJobStatus(existing, name), nil
}

func (e *JobExecutor) checkJobStatus(job *batchv1.Job, name string) *Result {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return &Result{Completed: true, Succeeded: true, JobName: name, Message: "Job completed successfully"}
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return &Result{Completed: true, Succeeded: false, JobName: name, Message: fmt.Sprintf("Job failed: %s", cond.Message)}
		}
	}

	// Still running
	return &Result{Completed: false, JobName: name, Message: "Job running"}
}

func (e *JobExecutor) buildJob(name, namespace string, spec *failoverv1alpha1.JobAction, ownerPrefix string, ownerRef *metav1.OwnerReference) *batchv1.Job {
	activeDeadline := defaultActiveDeadlineSeconds
	if spec.ActiveDeadlineSeconds != nil {
		activeDeadline = *spec.ActiveDeadlineSeconds
	}

	backoffLimit := defaultBackoffLimit
	if spec.MaxRetries != nil {
		backoffLimit = *spec.MaxRetries
	}

	command := spec.Command
	if spec.Script != nil {
		command = []string{"/bin/sh", "-c", *spec.Script}
	}

	envVars := make([]corev1.EnvVar, len(spec.Env))
	for i, e := range spec.Env {
		envVars[i] = corev1.EnvVar{Name: e.Name, Value: e.Value}
	}

	objectMeta := metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "k8s-failover-manager",
			"k8s-failover.zyno.io/owner":   truncateLabel(ownerPrefix),
		},
	}
	if ownerRef != nil {
		objectMeta.OwnerReferences = []metav1.OwnerReference{*ownerRef}
	}

	job := &batchv1.Job{
		ObjectMeta: objectMeta,
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &activeDeadline,
			BackoffLimit:          &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					Containers: []corev1.Container{
						{
							Name:    "action",
							Image:   spec.Image,
							Command: command,
							Env:     envVars,
						},
					},
				},
			},
		},
	}

	return job
}

// Cleanup deletes the Job and its pods.
func (e *JobExecutor) Cleanup(ctx context.Context, name string, namespace string) error {
	job := &batchv1.Job{}
	err := e.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, job)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting job for cleanup: %w", err)
	}

	propagation := metav1.DeletePropagationBackground
	return e.Client.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
	})
}
