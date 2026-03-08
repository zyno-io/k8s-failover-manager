package main

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	k8sfailoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

func TestSchemeIncludesRequiredTypes(t *testing.T) {
	tests := []schema.GroupVersionKind{
		batchv1.SchemeGroupVersion.WithKind("Job"),
		k8sfailoverv1alpha1.GroupVersion.WithKind("FailoverService"),
		k8sfailoverv1alpha1.GroupVersion.WithKind("FailoverEvent"),
	}

	for _, gvk := range tests {
		if !scheme.Recognizes(gvk) {
			t.Fatalf("scheme does not recognize %s", gvk.String())
		}
	}
}
