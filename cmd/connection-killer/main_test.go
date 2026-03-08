//go:build linux

package main

import "testing"

func TestAgentNodeName(t *testing.T) {
	t.Setenv("NODE_NAME", "node-a")

	nodeName, err := agentNodeName()
	if err != nil {
		t.Fatalf("agentNodeName error: %v", err)
	}
	if nodeName != "node-a" {
		t.Fatalf("expected node-a, got %q", nodeName)
	}
}

func TestAgentNodeNameMissing(t *testing.T) {
	t.Setenv("NODE_NAME", "")

	if _, err := agentNodeName(); err == nil {
		t.Fatal("expected error when NODE_NAME is missing")
	}
}

func TestControllerNamespaceDefault(t *testing.T) {
	t.Setenv("CONTROLLER_NAMESPACE", "")

	if got := controllerNamespace(); got != defaultControllerNamespace {
		t.Fatalf("expected default namespace %q, got %q", defaultControllerNamespace, got)
	}
}

func TestControllerNamespaceFromEnv(t *testing.T) {
	t.Setenv("CONTROLLER_NAMESPACE", "custom-ns")

	if got := controllerNamespace(); got != "custom-ns" {
		t.Fatalf("expected custom namespace, got %q", got)
	}
}
