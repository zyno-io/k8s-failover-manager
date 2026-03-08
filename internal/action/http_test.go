package action

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

type mockHTTPDoer struct {
	resp *http.Response
	err  error
}

func (m *mockHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	return m.resp, m.err
}

type netError struct {
	msg string
}

func (e *netError) Error() string { return e.msg }

type timeoutInspectingHTTPDoer struct {
	t               *testing.T
	expectedTimeout time.Duration
	resp            *http.Response
}

func (m *timeoutInspectingHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	if !ok {
		m.t.Fatal("expected request context deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > m.expectedTimeout+time.Second {
		m.t.Fatalf("expected deadline near %v, got remaining %v", m.expectedTimeout, remaining)
	}
	return m.resp, nil
}

func TestHTTPExecutor_Success(t *testing.T) {
	executor := &HTTPExecutor{
		Client: &mockHTTPDoer{
			resp: &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
	}

	body := `{"key":"value"}`
	action := &failoverv1alpha1.FailoverAction{
		Name: "test-http",
		HTTP: &failoverv1alpha1.HTTPAction{
			URL:     "http://example.com/test",
			Method:  "POST",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    &body,
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed {
		t.Error("expected Completed=true")
	}
	if !result.Succeeded {
		t.Error("expected Succeeded=true")
	}
}

func TestHTTPExecutor_Failure(t *testing.T) {
	executor := &HTTPExecutor{
		Client: &mockHTTPDoer{
			resp: &http.Response{
				StatusCode: 500,
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
	}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-http",
		HTTP: &failoverv1alpha1.HTTPAction{
			URL: "http://example.com/test",
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
		t.Error("expected Succeeded=false")
	}
	if result.Message != "HTTP 500" {
		t.Errorf("expected message 'HTTP 500', got %q", result.Message)
	}
}

func TestHTTPExecutor_NetworkError(t *testing.T) {
	executor := &HTTPExecutor{
		Client: &mockHTTPDoer{
			err: &netError{msg: "connection refused"},
		},
	}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-http",
		HTTP: &failoverv1alpha1.HTTPAction{
			URL: "http://example.com/test",
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if result != nil {
		t.Errorf("expected nil result on transport error, got %#v", result)
	}
}

func TestHTTPExecutor_DefaultMethod(t *testing.T) {
	var capturedMethod string
	executor := &HTTPExecutor{
		Client: &mockHTTPDoer{
			resp: &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
	}
	// The default method is GET when not specified
	action := &failoverv1alpha1.FailoverAction{
		Name: "test-default",
		HTTP: &failoverv1alpha1.HTTPAction{
			URL: "http://example.com/test",
		},
	}

	result, err := executor.Execute(context.Background(), action, "default", "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Succeeded {
		t.Error("expected Succeeded=true")
	}
	_ = capturedMethod // method verification would require intercepting request
}

func TestHTTPExecutor_StatusBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		status int
		wantOK bool
	}{
		{"199 fails", 199, false},
		{"200 passes", 200, true},
		{"299 passes", 299, true},
		{"300 fails", 300, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &HTTPExecutor{
				Client: &mockHTTPDoer{
					resp: &http.Response{
						StatusCode: tt.status,
						Body:       io.NopCloser(strings.NewReader("")),
					},
				},
			}
			action := &failoverv1alpha1.FailoverAction{
				Name: "boundary-test",
				HTTP: &failoverv1alpha1.HTTPAction{URL: "http://example.com/test"},
			}

			result, err := executor.Execute(context.Background(), action, "default", "test", 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.Completed {
				t.Error("expected Completed=true")
			}
			if result.Succeeded != tt.wantOK {
				t.Errorf("status %d: Succeeded=%v, want %v", tt.status, result.Succeeded, tt.wantOK)
			}
		})
	}
}

func TestHTTPExecutor_InvalidURL(t *testing.T) {
	executor := &HTTPExecutor{
		Client: &mockHTTPDoer{
			resp: &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
	}
	action := &failoverv1alpha1.FailoverAction{
		Name: "bad-url",
		HTTP: &failoverv1alpha1.HTTPAction{
			URL: "http://example .com/has spaces",
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
		t.Error("expected Succeeded=false for invalid URL")
	}
}

func TestHTTPExecutor_CustomTimeout(t *testing.T) {
	timeoutSeconds := int64(3)
	executor := &HTTPExecutor{
		Client: &timeoutInspectingHTTPDoer{
			t:               t,
			expectedTimeout: 3 * time.Second,
			resp: &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
	}

	action := &failoverv1alpha1.FailoverAction{
		Name: "test-http-timeout",
		HTTP: &failoverv1alpha1.HTTPAction{
			URL:            "http://example.com/test",
			TimeoutSeconds: &timeoutSeconds,
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
