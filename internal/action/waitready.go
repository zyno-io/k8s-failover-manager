package action

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

// WaitReadyExecutor polls until a Deployment or StatefulSet is fully ready.
type WaitReadyExecutor struct {
	Client client.Client
}

func (e *WaitReadyExecutor) Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error) {
	spec := action.WaitReady
	key := types.NamespacedName{Name: spec.Name, Namespace: namespace}

	switch spec.Kind {
	case "Deployment":
		return e.checkDeployment(ctx, key)
	case "StatefulSet":
		return e.checkStatefulSet(ctx, key)
	default:
		return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("unsupported kind %q", spec.Kind)}, nil
	}
}

func (e *WaitReadyExecutor) checkDeployment(ctx context.Context, key types.NamespacedName) (*Result, error) {
	dep := &appsv1.Deployment{}
	if err := e.Client.Get(ctx, key, dep); err != nil {
		if errors.IsNotFound(err) {
			return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("Deployment %s not found", key.Name)}, nil
		}
		return nil, fmt.Errorf("getting deployment %s: %w", key.Name, err)
	}

	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}

	if dep.Status.ReadyReplicas == desired &&
		dep.Status.UpdatedReplicas == desired &&
		dep.Status.AvailableReplicas == desired &&
		dep.Status.ObservedGeneration == dep.Generation {
		return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("Deployment %s is ready (%d/%d)", key.Name, dep.Status.ReadyReplicas, desired)}, nil
	}

	return &Result{Completed: false, Message: fmt.Sprintf("Deployment %s not ready yet (%d/%d)", key.Name, dep.Status.ReadyReplicas, desired)}, nil
}

func (e *WaitReadyExecutor) checkStatefulSet(ctx context.Context, key types.NamespacedName) (*Result, error) {
	sts := &appsv1.StatefulSet{}
	if err := e.Client.Get(ctx, key, sts); err != nil {
		if errors.IsNotFound(err) {
			return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("StatefulSet %s not found", key.Name)}, nil
		}
		return nil, fmt.Errorf("getting statefulset %s: %w", key.Name, err)
	}

	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}

	if sts.Status.ReadyReplicas == desired &&
		sts.Status.UpdatedReplicas == desired &&
		sts.Status.AvailableReplicas == desired &&
		sts.Status.ObservedGeneration == sts.Generation {
		return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("StatefulSet %s is ready (%d/%d)", key.Name, sts.Status.ReadyReplicas, desired)}, nil
	}

	return &Result{Completed: false, Message: fmt.Sprintf("StatefulSet %s not ready yet (%d/%d)", key.Name, sts.Status.ReadyReplicas, desired)}, nil
}

// CheckTimeout checks if startedAt + timeout has elapsed. Used by the controller.
func CheckTimeout(startedAt *metav1.Time, timeoutSec int64) bool {
	if startedAt == nil {
		return false
	}
	deadline := startedAt.Add(time.Duration(timeoutSec) * time.Second)
	return time.Now().After(deadline)
}
