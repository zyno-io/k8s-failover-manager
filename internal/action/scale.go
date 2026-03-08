package action

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

// ScaleExecutor executes scale actions on Deployments and StatefulSets.
type ScaleExecutor struct {
	Client    client.Client
	WaitReady *WaitReadyExecutor
}

func (e *ScaleExecutor) Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error) {
	spec := action.Scale

	obj, err := e.getScalableObject(ctx, spec.Kind, spec.Name, namespace)
	if err != nil {
		if errors.IsNotFound(err) {
			return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("%s %s not found", spec.Kind, spec.Name)}, nil
		}
		return nil, fmt.Errorf("getting %s %s: %w", spec.Kind, spec.Name, err)
	}

	// Get current scale
	scale := &autoscalingv1.Scale{}
	if err := e.Client.SubResource("scale").Get(ctx, obj, scale); err != nil {
		return nil, fmt.Errorf("getting scale for %s %s: %w", spec.Kind, spec.Name, err)
	}

	// Update scale if needed
	if scale.Spec.Replicas != spec.Replicas {
		scale.Spec.Replicas = spec.Replicas
		if err := e.Client.SubResource("scale").Update(ctx, obj, client.WithSubResourceBody(scale)); err != nil {
			return nil, fmt.Errorf("updating scale for %s %s: %w", spec.Kind, spec.Name, err)
		}
	}

	// If waitReady is requested, delegate to the WaitReadyExecutor
	if spec.WaitReady && e.WaitReady == nil {
		return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("scale.waitReady requested for %s %s but WaitReady executor is not configured", spec.Kind, spec.Name)}, nil
	}
	if spec.WaitReady && e.WaitReady != nil {
		waitAction := &failoverv1alpha1.FailoverAction{
			Name: action.Name,
			WaitReady: &failoverv1alpha1.WaitReadyAction{
				Kind: spec.Kind,
				Name: spec.Name,
			},
		}
		return e.WaitReady.Execute(ctx, waitAction, namespace, namePrefix, actionIndex)
	}

	return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("Scaled %s %s to %d", spec.Kind, spec.Name, spec.Replicas)}, nil
}

func (e *ScaleExecutor) getScalableObject(ctx context.Context, kind, name, namespace string) (client.Object, error) {
	key := types.NamespacedName{Name: name, Namespace: namespace}
	switch kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		return obj, e.Client.Get(ctx, key, obj)
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		return obj, e.Client.Get(ctx, key, obj)
	default:
		return nil, fmt.Errorf("unsupported kind %q, must be Deployment or StatefulSet", kind)
	}
}
