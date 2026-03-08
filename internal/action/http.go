package action

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

const defaultHTTPTimeout = 30 * time.Second

// HTTPDoer abstracts HTTP client for testability.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// HTTPExecutor executes HTTP actions.
type HTTPExecutor struct {
	Client HTTPDoer
}

func (e *HTTPExecutor) Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error) {
	spec := action.HTTP

	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if spec.Body != nil {
		bodyReader = bytes.NewBufferString(*spec.Body)
	}

	timeout := defaultHTTPTimeout
	if spec.TimeoutSeconds != nil {
		timeout = time.Duration(*spec.TimeoutSeconds) * time.Second
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, method, spec.URL, bodyReader)
	if err != nil {
		return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("building request: %v", err)}, nil
	}

	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}

	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		// Transport-level failures are treated as transient execution errors so the
		// controller retry/backoff policy can decide whether to retry or fail.
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}

	return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
}
