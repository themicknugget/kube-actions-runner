package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v57/github"
)

func newTestClient(serverURL string) *Client {
	return newTestClientWithGroupID(serverURL, 1)
}

func newTestClientWithGroupID(serverURL string, runnerGroupID int64) *Client {
	client := github.NewClient(nil)
	baseURL, _ := client.BaseURL.Parse(serverURL + "/")
	client.BaseURL = baseURL
	return &Client{client: client, runnerGroupID: runnerGroupID}
}

func TestGenerateJITConfig_Success(t *testing.T) {
	expectedJITConfig := "base64-encoded-jit-config"
	expectedRunnerID := int64(12345)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST method, got %s", r.Method)
		}
		expectedPath := "/repos/test-owner/test-repo/actions/runners/generate-jitconfig"
		if !strings.HasSuffix(r.URL.Path, expectedPath) {
			t.Errorf("expected path %s, got %s", expectedPath, r.URL.Path)
		}

		var req github.GenerateJITConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}
		if req.Name != "test-runner" {
			t.Errorf("expected runner name 'test-runner', got %s", req.Name)
		}
		if req.RunnerGroupID != 1 {
			t.Errorf("expected runner group ID 1, got %d", req.RunnerGroupID)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"encoded_jit_config": expectedJITConfig,
			"runner": map[string]interface{}{
				"id":   expectedRunnerID,
				"name": "test-runner",
			},
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	config, err := client.GenerateJITConfig(ctx, "test-owner", "test-repo", "test-runner", []string{"self-hosted", "linux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if config.EncodedJITConfig != expectedJITConfig {
		t.Errorf("expected JIT config %q, got %q", expectedJITConfig, config.EncodedJITConfig)
	}
	if config.RunnerID != expectedRunnerID {
		t.Errorf("expected runner ID %d, got %d", expectedRunnerID, config.RunnerID)
	}
}

func TestGenerateJITConfig_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Validation Failed",
			"errors": []map[string]string{
				{"resource": "Runner", "code": "already_exists", "field": "name"},
			},
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	_, err := client.GenerateJITConfig(ctx, "test-owner", "test-repo", "test-runner", []string{"self-hosted"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "failed to generate JIT config") {
		t.Errorf("expected error message to contain 'failed to generate JIT config', got: %v", err)
	}
}

func TestGenerateJITConfig_RateLimitTracking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4500")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"encoded_jit_config": "config",
			"runner": map[string]interface{}{
				"id":   1,
				"name": "runner",
			},
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	_, err := client.GenerateJITConfig(ctx, "test-owner", "test-repo", "test-runner", []string{"self-hosted"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsJobQueued_ReturnsTrue_WhenStatusIsQueued(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET method, got %s", r.Method)
		}
		expectedPath := "/repos/test-owner/test-repo/actions/jobs/12345"
		if !strings.HasSuffix(r.URL.Path, expectedPath) {
			t.Errorf("expected path %s, got %s", expectedPath, r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4998")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     12345,
			"status": "queued",
			"name":   "test-job",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	isQueued, err := client.IsJobQueued(ctx, "test-owner", "test-repo", 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !isQueued {
		t.Error("expected IsJobQueued to return true for status 'queued'")
	}
}

func TestIsJobQueued_ReturnsFalse_WhenStatusIsInProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4997")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     12345,
			"status": "in_progress",
			"name":   "test-job",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	isQueued, err := client.IsJobQueued(ctx, "test-owner", "test-repo", 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if isQueued {
		t.Error("expected IsJobQueued to return false for status 'in_progress'")
	}
}

func TestIsJobQueued_ReturnsFalse_WhenStatusIsCompleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4996")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         12345,
			"status":     "completed",
			"conclusion": "success",
			"name":       "test-job",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	isQueued, err := client.IsJobQueued(ctx, "test-owner", "test-repo", 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if isQueued {
		t.Error("expected IsJobQueued to return false for status 'completed'")
	}
}

func TestIsJobQueued_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":           "Not Found",
			"documentation_url": "https://docs.github.com/rest",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	isQueued, err := client.IsJobQueued(ctx, "test-owner", "test-repo", 99999)

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if isQueued {
		t.Error("expected IsJobQueued to return false on error")
	}

	if !strings.Contains(err.Error(), "failed to get workflow job") {
		t.Errorf("expected error message to contain 'failed to get workflow job', got: %v", err)
	}
}

func TestIsJobQueued_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Internal Server Error",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	isQueued, err := client.IsJobQueued(ctx, "test-owner", "test-repo", 12345)

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if isQueued {
		t.Error("expected IsJobQueued to return false on server error")
	}
}

func TestGenerateJITConfig_LabelsPassedCorrectly(t *testing.T) {
	expectedLabels := []string{"self-hosted", "linux", "x64", "custom-label"}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req github.GenerateJITConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request body: %v", err)
		}

		if len(req.Labels) != len(expectedLabels) {
			t.Errorf("expected %d labels, got %d", len(expectedLabels), len(req.Labels))
		}
		for i, label := range expectedLabels {
			if req.Labels[i] != label {
				t.Errorf("expected label %q at index %d, got %q", label, i, req.Labels[i])
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"encoded_jit_config": "config",
			"runner": map[string]interface{}{
				"id":   1,
				"name": "runner",
			},
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	_, err := client.GenerateJITConfig(ctx, "test-owner", "test-repo", "test-runner", expectedLabels)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetWorkflowJobStatus_ReturnsQueuedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4998")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     12345,
			"status": "queued",
			"name":   "test-job",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	status, err := client.GetWorkflowJobStatus(ctx, "test-owner", "test-repo", 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if status == nil {
		t.Fatal("expected status, got nil")
	}
	if status.Status != "queued" {
		t.Errorf("expected status 'queued', got %q", status.Status)
	}
	if status.Conclusion != "" {
		t.Errorf("expected empty conclusion, got %q", status.Conclusion)
	}
}

func TestGetWorkflowJobStatus_ReturnsCompletedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4997")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         12345,
			"status":     "completed",
			"conclusion": "cancelled",
			"name":       "test-job",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	status, err := client.GetWorkflowJobStatus(ctx, "test-owner", "test-repo", 12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if status == nil {
		t.Fatal("expected status, got nil")
	}
	if status.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", status.Status)
	}
	if status.Conclusion != "cancelled" {
		t.Errorf("expected conclusion 'cancelled', got %q", status.Conclusion)
	}
}

func TestGetWorkflowJobStatus_ReturnsNilOnNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":           "Not Found",
			"documentation_url": "https://docs.github.com/rest",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	status, err := client.GetWorkflowJobStatus(ctx, "test-owner", "test-repo", 99999)
	if err != nil {
		t.Fatalf("unexpected error on 404: %v", err)
	}

	if status != nil {
		t.Errorf("expected nil status for 404, got %+v", status)
	}
}

func TestGetWorkflowJobStatus_ReturnsErrorOnServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Internal Server Error",
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	ctx := context.Background()

	status, err := client.GetWorkflowJobStatus(ctx, "test-owner", "test-repo", 12345)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if status != nil {
		t.Error("expected nil status on error")
	}
}
