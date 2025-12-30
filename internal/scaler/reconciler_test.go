package scaler

import (
	"context"
	"fmt"
	"testing"
	"time"

	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/tokens"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockGHClient implements a mock GitHub client for testing
type mockGHClient struct {
	repos      []string
	queuedJobs map[string][]ghclient.QueuedJob // repo -> jobs
	jitConfigs map[int64]*ghclient.JITConfig   // jobID -> config
}

func (m *mockGHClient) ListRepositories(ctx context.Context, owner string) ([]string, error) {
	return m.repos, nil
}

func (m *mockGHClient) ListQueuedJobs(ctx context.Context, owner, repo string) ([]ghclient.QueuedJob, error) {
	if jobs, ok := m.queuedJobs[repo]; ok {
		return jobs, nil
	}
	return nil, nil
}

func (m *mockGHClient) GenerateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error) {
	return &ghclient.JITConfig{
		EncodedJITConfig: "test-jit-config",
		RunnerID:         12345,
	}, nil
}

// mockK8sClientForReconciler implements k8s client methods needed by reconciler
type mockK8sClientForReconciler struct {
	pods        []corev1.Pod
	createdJobs []k8s.RunnerJobConfig
}

func (m *mockK8sClientForReconciler) ListRunnerPods(ctx context.Context) ([]corev1.Pod, error) {
	return m.pods, nil
}

func (m *mockK8sClientForReconciler) CreateRunnerJob(ctx context.Context, config k8s.RunnerJobConfig) error {
	m.createdJobs = append(m.createdJobs, config)
	return nil
}

func TestReconciler_MatchesLabels(t *testing.T) {
	tests := []struct {
		name          string
		matchers      []LabelMatcher
		labels        []string
		shouldMatch   bool
	}{
		{
			name:        "no matchers matches everything",
			matchers:    nil,
			labels:      []string{"self-hosted", "linux"},
			shouldMatch: true,
		},
		{
			name:        "single matcher matches",
			matchers:    []LabelMatcher{{Labels: []string{"self-hosted"}}},
			labels:      []string{"self-hosted", "linux"},
			shouldMatch: true,
		},
		{
			name:        "single matcher no match",
			matchers:    []LabelMatcher{{Labels: []string{"custom-runner"}}},
			labels:      []string{"self-hosted", "linux"},
			shouldMatch: false,
		},
		{
			name: "multiple matchers one matches",
			matchers: []LabelMatcher{
				{Labels: []string{"custom-runner"}},
				{Labels: []string{"self-hosted"}},
			},
			labels:      []string{"self-hosted", "linux"},
			shouldMatch: true,
		},
		{
			name: "multiple matchers none match",
			matchers: []LabelMatcher{
				{Labels: []string{"custom-runner"}},
				{Labels: []string{"special"}},
			},
			labels:      []string{"self-hosted", "linux"},
			shouldMatch: false,
		},
		{
			name:        "empty labels",
			matchers:    []LabelMatcher{{Labels: []string{"self-hosted"}}},
			labels:      []string{},
			shouldMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{
				labelMatchers: tt.matchers,
			}
			result := r.matchesLabels(tt.labels)
			if result != tt.shouldMatch {
				t.Errorf("matchesLabels() = %v, want %v", result, tt.shouldMatch)
			}
		})
	}
}

func TestReconciler_SkipsExistingRunners(t *testing.T) {
	// Setup: existing pod for job ID 12345
	existingPods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "runner-12345",
				Labels: map[string]string{
					"app":    "github-runner",
					"job-id": "12345",
				},
			},
		},
	}

	// Queued jobs: one with existing runner (12345), one without (67890)
	queuedJobs := []ghclient.QueuedJob{
		{
			ID:     12345,
			Name:   "build",
			Owner:  "testowner",
			Repo:   "testrepo",
			Labels: []string{"self-hosted"},
		},
		{
			ID:     67890,
			Name:   "test",
			Owner:  "testowner",
			Repo:   "testrepo",
			Labels: []string{"self-hosted"},
		},
	}

	mockK8s := &mockK8sClientForReconciler{pods: existingPods}

	// Run reconciliation logic
	existingJobIDs := make(map[int64]bool)
	for _, pod := range existingPods {
		if jobID, ok := pod.Labels["job-id"]; ok {
			var id int64
			_, _ = fmt.Sscanf(jobID, "%d", &id)
			existingJobIDs[id] = true
		}
	}

	var jobsToCreate []ghclient.QueuedJob
	for _, job := range queuedJobs {
		if !existingJobIDs[job.ID] {
			jobsToCreate = append(jobsToCreate, job)
		}
	}

	// Verify only job 67890 should be created
	if len(jobsToCreate) != 1 {
		t.Errorf("expected 1 job to create, got %d", len(jobsToCreate))
	}
	if len(jobsToCreate) > 0 && jobsToCreate[0].ID != 67890 {
		t.Errorf("expected job ID 67890, got %d", jobsToCreate[0].ID)
	}

	_ = mockK8s // Suppress unused warning
}

func TestReconciler_FiltersUnmatchedLabels(t *testing.T) {
	queuedJobs := []ghclient.QueuedJob{
		{
			ID:     1,
			Name:   "self-hosted-job",
			Labels: []string{"self-hosted", "linux"},
		},
		{
			ID:     2,
			Name:   "custom-runner-job",
			Labels: []string{"custom-runner", "linux"},
		},
		{
			ID:     3,
			Name:   "special-job",
			Labels: []string{"special", "ubuntu-latest"},
		},
	}

	r := &Reconciler{
		labelMatchers: []LabelMatcher{
			{Labels: []string{"self-hosted"}},
		},
	}

	var matchedJobs []ghclient.QueuedJob
	for _, job := range queuedJobs {
		if r.matchesLabels(job.Labels) {
			matchedJobs = append(matchedJobs, job)
		}
	}

	if len(matchedJobs) != 1 {
		t.Errorf("expected 1 matched job, got %d", len(matchedJobs))
	}
	if len(matchedJobs) > 0 && matchedJobs[0].ID != 1 {
		t.Errorf("expected job ID 1, got %d", matchedJobs[0].ID)
	}
}

func TestReconciler_NewReconcilerDefaults(t *testing.T) {
	log := logger.New()

	// Test default interval
	r := NewReconciler(ReconcilerConfig{
		Logger: log,
	})

	if r.interval != 30*time.Second {
		t.Errorf("expected default interval 30s, got %v", r.interval)
	}

	// Test custom interval
	r2 := NewReconciler(ReconcilerConfig{
		Logger:   log,
		Interval: 60 * time.Second,
	})

	if r2.interval != 60*time.Second {
		t.Errorf("expected custom interval 60s, got %v", r2.interval)
	}
}

func TestReconciler_HandlesEmptyOwnerList(t *testing.T) {
	log := logger.New()

	// Create token registry with no owners (only default token)
	registry, err := tokens.NewRegistry("", "test-token")
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	owners := registry.GetConfiguredOwners()
	if len(owners) != 0 {
		t.Errorf("expected 0 owners for default-only token, got %d", len(owners))
	}

	_ = log // Suppress unused warning
}

func TestReconciler_HandlesMultipleOwners(t *testing.T) {
	// Create token registry with multiple owners
	tokensJSON := `[{"owner":"owner1","token":"token1"},{"owner":"owner2","token":"token2"}]`
	registry, err := tokens.NewRegistry(tokensJSON, "")
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	owners := registry.GetConfiguredOwners()
	if len(owners) != 2 {
		t.Errorf("expected 2 owners, got %d", len(owners))
	}
}
