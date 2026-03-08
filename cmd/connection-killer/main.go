//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/zyno-io/k8s-failover-manager/internal/connkill"
)

const defaultControllerNamespace = "k8s-failover-manager-system"

func agentNodeName() (string, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return "", fmt.Errorf("NODE_NAME environment variable is required")
	}
	return nodeName, nil
}

func controllerNamespace() string {
	namespace := os.Getenv("CONTROLLER_NAMESPACE")
	if namespace == "" {
		return defaultControllerNamespace
	}
	return namespace
}

func main() {
	opts := zap.Options{Development: false}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("connection-killer")

	nodeName, err := agentNodeName()
	if err != nil {
		log.Error(err, "NODE_NAME environment variable is required")
		os.Exit(1)
	}

	namespace := controllerNamespace()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "Failed to create client")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "Failed to create clientset")
		os.Exit(1)
	}

	agent := &connkill.Agent{
		Client:    c,
		Clientset: clientset,
		NodeName:  nodeName,
		Namespace: namespace,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log.Info("Starting connection killer agent", "node", nodeName, "namespace", namespace)
	if err := agent.Watch(ctx, 1*time.Second); err != nil && err != context.Canceled {
		log.Error(err, "Agent watch failed")
		os.Exit(1)
	}
}
