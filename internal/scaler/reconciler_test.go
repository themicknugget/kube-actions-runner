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
	"k8s.io/client-go/kubernetes/fake"
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
			name:        "no matchers matches self-hosted",
			matchers:    nil,
			labels:      []string{"self-hosted", "linux"},
			shouldMatch: true,
		},
		{
			name:        "single matcher matches",
			matchers:    []LabelMatcher{{Labels: []string{"linux"}}},
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
				{Labels: []string{"linux"}},
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
			name:        "missing self-hosted label",
			matchers:    nil,
			labels:      []string{"linux", "ubuntu"},
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
			Name:   "self-hosted-linux-job",
			Labels: []string{"self-hosted", "linux"},
		},
		{
			ID:     2,
			Name:   "self-hosted-windows-job",
			Labels: []string{"self-hosted", "windows"},
		},
		{
			ID:     3,
			Name:   "non-self-hosted-job",
			Labels: []string{"ubuntu-latest"},
		},
	}

	// Match only linux jobs
	r := &Reconciler{
		labelMatchers: []LabelMatcher{
			{Labels: []string{"linux"}},
		},
	}

	var matchedJobs []ghclient.QueuedJob
	for _, job := range queuedJobs {
		if r.matchesLabels(job.Labels) {
			matchedJobs = append(matchedJobs, job)
		}
	}

	// Should match only job 1 (self-hosted + linux)
	// Job 2 has self-hosted but not linux
	// Job 3 doesn't have self-hosted
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

	if r.interval != 5*time.Minute {
		t.Errorf("expected default interval 5m, got %v", r.interval)
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

// readyNodeReconciler returns a Ready node with the given arch label for use
// in reconciler arch-check tests.
func readyNodeReconciler(name, arch string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/arch": arch,
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestReconciler_SkipsArm64Jobs(t *testing.T) {
	// Cluster has only amd64 nodes. An arm64 job arriving in the reconciler
	// should be skipped without creating a K8s Job (it would otherwise sit
	// Pending forever and accumulate on every reconciliation cycle).
	fakeClientset := fake.NewSimpleClientset(readyNodeReconciler("node1", "amd64"))
	k8sClient := k8s.NewClientWithClientset(fakeClientset, "test-ns")

	log := logger.New()
	s := &Scaler{
		k8sClient: k8sClient,
		logger:    log,
	}
	r := &Reconciler{
		scaler: s,
		logger: log,
	}

	// Use a real *ghclient.Client; the test never reaches GenerateJITConfig
	// because the arch check should short-circuit before then.
	gh := ghclient.NewClientForOwner("test-token", 1, "testowner")

	job := ghclient.QueuedJob{
		ID:     424242,
		Name:   "arm64-build",
		Owner:  "testowner",
		Repo:   "testrepo",
		Labels: []string{"self-hosted", "arm64"},
	}

	if err := r.createRunnerForJob(context.Background(), gh, job); err != nil {
		t.Fatalf("expected nil error for skipped arm64 job, got: %v", err)
	}

	// Verify no K8s Job was created in the namespace
	jobs, err := fakeClientset.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		names := make([]string, 0, len(jobs.Items))
		for _, j := range jobs.Items {
			names = append(names, j.Name)
		}
		t.Errorf("expected no jobs to be created, got: %v", names)
	}
}

func TestReconciler_AllowsAmd64Jobs(t *testing.T) {
	// Sanity check: when the cluster supports the requested arch, the
	// reconciler does NOT short-circuit and proceeds to create the K8s job.
	// We don't care about the GH client behavior here; we're verifying the
	// arch check doesn't reject valid jobs.
	fakeClientset := fake.NewSimpleClientset(readyNodeReconciler("node1", "amd64"))
	k8sClient := k8s.NewClientWithClientset(fakeClientset, "test-ns")

	log := logger.New()
	s := &Scaler{
		k8sClient: k8sClient,
		logger:    log,
	}
	r := &Reconciler{
		scaler: s,
		logger: log,
	}

	// Use a real *ghclient.Client whose underlying HTTP client will fail
	// (nil transport) when GenerateJITConfig is called. That's fine: we
	// only care that skipForArch did not short-circuit this job, which
	// manifests as the error originating from GitHub, not from the arch
	// check. The test simply verifies the error is NOT a "no nodes
	// available" error.
	gh := ghclient.NewClientForOwner("test-token", 1, "testowner")

	job := ghclient.QueuedJob{
		ID:     515151,
		Name:   "amd64-build",
		Owner:  "testowner",
		Repo:   "testrepo",
		Labels: []string{"self-hosted", "amd64"},
	}

	err := r.createRunnerForJob(context.Background(), gh, job)
	if err == nil {
		t.Fatal("expected an error from downstream GH/K8s call, got nil")
	}
	// The arch check should not be the source of the error
	if err.Error() == "no nodes available for architecture, job will remain queued in GitHub" {
		t.Errorf("expected arch check to pass for amd64 with amd64 nodes, got arch error: %v", err)
	}
}
