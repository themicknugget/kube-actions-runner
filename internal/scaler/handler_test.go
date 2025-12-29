package scaler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
)

type GitHubClient interface {
	GenerateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error)
	IsJobQueued(ctx context.Context, owner, repo string, jobID int64) (bool, error)
}

type K8sClient interface {
	JobExists(ctx context.Context, name string) (bool, error)
	CreateRunnerJob(ctx context.Context, config k8s.RunnerJobConfig) error
}

type mockGitHubClient struct {
	generateJITConfigFunc func(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error)
	isJobQueuedFunc       func(ctx context.Context, owner, repo string, jobID int64) (bool, error)
}

func (m *mockGitHubClient) GenerateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error) {
	if m.generateJITConfigFunc != nil {
		return m.generateJITConfigFunc(ctx, owner, repo, runnerName, labels)
	}
	return &ghclient.JITConfig{EncodedJITConfig: "test-config", RunnerID: 12345}, nil
}

func (m *mockGitHubClient) IsJobQueued(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
	if m.isJobQueuedFunc != nil {
		return m.isJobQueuedFunc(ctx, owner, repo, jobID)
	}
	return true, nil
}

type mockK8sClient struct {
	jobExistsFunc       func(ctx context.Context, name string) (bool, error)
	createRunnerJobFunc func(ctx context.Context, config k8s.RunnerJobConfig) error
}

func (m *mockK8sClient) JobExists(ctx context.Context, name string) (bool, error) {
	if m.jobExistsFunc != nil {
		return m.jobExistsFunc(ctx, name)
	}
	return false, nil
}

func (m *mockK8sClient) CreateRunnerJob(ctx context.Context, config k8s.RunnerJobConfig) error {
	if m.createRunnerJobFunc != nil {
		return m.createRunnerJobFunc(ctx, config)
	}
	return nil
}

type testScaler struct {
	webhookSecret []byte
	labelMatchers []LabelMatcher
	ghClient      GitHubClient
	k8sClient     K8sClient
	logger        *logger.Logger
	runnerMode    k8s.RunnerMode
	runnerImage   string
	dindImage     string
}

func newTestScaler(ghClient GitHubClient, k8sClient K8sClient) *testScaler {
	log := logger.New()
	log.SetLevel(logger.LevelError)
	return &testScaler{
		webhookSecret: []byte("test-secret"),
		labelMatchers: []LabelMatcher{
			{Labels: []string{"self-hosted"}, ExactMatch: false},
		},
		ghClient:    ghClient,
		k8sClient:   k8sClient,
		logger:      log,
		runnerMode:  k8s.RunnerModeStandard,
		runnerImage: "test-image",
		dindImage:   "test-dind-image",
	}
}

func (s *testScaler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !ValidateSignature(s.webhookSecret, r) {
		s.logger.Warn("invalid webhook signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var event workflowJobEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		s.logger.Error("failed to parse webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	owner := event.Repo.Owner.Login
	repo := event.Repo.Name
	jobID := event.WorkflowJob.ID
	action := event.Action

	log := s.logger.With(
		"owner", owner,
		"repo", repo,
		"job_id", jobID,
		"action", action,
	)

	if action != "queued" {
		log.Debug("ignoring non-queued event")
		w.WriteHeader(http.StatusOK)
		return
	}

	jobLabels := event.WorkflowJob.Labels
	if !ShouldHandle(jobLabels, s.labelMatchers) {
		log.Debug("job labels do not match", "labels", jobLabels)
		w.WriteHeader(http.StatusOK)
		return
	}

	log.Info("processing workflow job", "labels", jobLabels)

	jobName := "runner-" + string(rune(jobID))

	exists, err := s.k8sClient.JobExists(ctx, jobName)
	if err != nil {
		log.Error("failed to check job existence", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if exists {
		log.Info("job already exists, skipping")
		w.WriteHeader(http.StatusOK)
		return
	}

	stillQueued, err := s.ghClient.IsJobQueued(ctx, owner, repo, jobID)
	if err != nil {
		log.Error("failed to verify job status", "error", err)
		http.Error(w, "failed to verify job status", http.StatusInternalServerError)
		return
	}
	if !stillQueued {
		log.Info("job no longer queued, skipping")
		w.WriteHeader(http.StatusOK)
		return
	}

	runnerName := "runner-" + string(rune(jobID))
	jitConfig, err := s.ghClient.GenerateJITConfig(ctx, owner, repo, runnerName, jobLabels)
	if err != nil {
		log.Error("failed to generate JIT config", "error", err)
		http.Error(w, "failed to generate JIT config", http.StatusInternalServerError)
		return
	}

	log.Info("generated JIT config", "runner_id", jitConfig.RunnerID)

	config := k8s.RunnerJobConfig{
		Name:        jobName,
		JITConfig:   jitConfig.EncodedJITConfig,
		Owner:       owner,
		Repo:        repo,
		WorkflowID:  jobID,
		Labels:      jobLabels,
		RunnerMode:  s.runnerMode,
		RunnerImage: s.runnerImage,
		DindImage:   s.dindImage,
	}
	if err := s.k8sClient.CreateRunnerJob(ctx, config); err != nil {
		log.Error("failed to create runner job", "error", err)
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}

	log.Info("created runner job", "job_name", jobName, "runner_mode", s.runnerMode)
	w.WriteHeader(http.StatusOK)
}

type workflowJobEvent struct {
	Action      string      `json:"action"`
	WorkflowJob workflowJob `json:"workflow_job"`
	Repo        repository  `json:"repository"`
}

type workflowJob struct {
	ID     int64    `json:"id"`
	Labels []string `json:"labels"`
}

type repository struct {
	Name  string    `json:"name"`
	Owner repoOwner `json:"owner"`
}

type repoOwner struct {
	Login string `json:"login"`
}

func createSignedRequest(secret []byte, body []byte) *http.Request {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func createWebhookPayload(action string, jobID int64, owner, repo string, labels []string) []byte {
	event := workflowJobEvent{
		Action: action,
		WorkflowJob: workflowJob{
			ID:     jobID,
			Labels: labels,
		},
		Repo: repository{
			Name: repo,
			Owner: repoOwner{
				Login: owner,
			},
		},
	}
	data, _ := json.Marshal(event)
	return data
}

func TestHandleWebhook_InvalidSignature(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidsignature")
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestHandleWebhook_InvalidJSON(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{}
	scaler := newTestScaler(ghClient, k8sClient)

	invalidJSON := []byte(`{"action": "queued", "invalid json`)

	req := createSignedRequest(scaler.webhookSecret, invalidJSON)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestHandleWebhook_NonQueuedAction(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{}
	scaler := newTestScaler(ghClient, k8sClient)

	// Test "completed" action
	body := createWebhookPayload("completed", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d for completed action, got %d", http.StatusOK, rr.Code)
	}

	// Test "in_progress" action
	body = createWebhookPayload("in_progress", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req = createSignedRequest(scaler.webhookSecret, body)
	rr = httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d for in_progress action, got %d", http.StatusOK, rr.Code)
	}
}

func TestHandleWebhook_LabelsNoMatch(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{}
	scaler := newTestScaler(ghClient, k8sClient)

	scaler.labelMatchers = []LabelMatcher{
		{Labels: []string{"custom-runner"}, ExactMatch: false},
	}

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted", "linux"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d when labels don't match, got %d", http.StatusOK, rr.Code)
	}
}

func TestHandleWebhook_JobAlreadyExists(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return true, nil
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d for existing job (idempotent), got %d", http.StatusOK, rr.Code)
	}
}

func TestHandleWebhook_SuccessfulJobCreation(t *testing.T) {
	var createdConfig k8s.RunnerJobConfig
	ghClient := &mockGitHubClient{
		isJobQueuedFunc: func(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
			return true, nil
		},
		generateJITConfigFunc: func(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error) {
			return &ghclient.JITConfig{
				EncodedJITConfig: "test-jit-config",
				RunnerID:         99999,
			}, nil
		},
	}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return false, nil
		},
		createRunnerJobFunc: func(ctx context.Context, config k8s.RunnerJobConfig) error {
			createdConfig = config
			return nil
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted", "linux"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d for successful job creation, got %d", http.StatusOK, rr.Code)
	}

	if createdConfig.Owner != "testowner" {
		t.Errorf("expected owner 'testowner', got '%s'", createdConfig.Owner)
	}
	if createdConfig.Repo != "testrepo" {
		t.Errorf("expected repo 'testrepo', got '%s'", createdConfig.Repo)
	}
	if createdConfig.JITConfig != "test-jit-config" {
		t.Errorf("expected JITConfig 'test-jit-config', got '%s'", createdConfig.JITConfig)
	}
	if createdConfig.WorkflowID != 12345 {
		t.Errorf("expected WorkflowID 12345, got %d", createdConfig.WorkflowID)
	}
}

func TestHandleWebhook_JobNoLongerQueued(t *testing.T) {
	var createRunnerJobCalled bool
	ghClient := &mockGitHubClient{
		isJobQueuedFunc: func(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
			return false, nil
		},
	}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return false, nil
		},
		createRunnerJobFunc: func(ctx context.Context, config k8s.RunnerJobConfig) error {
			createRunnerJobCalled = true
			return nil
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d when job no longer queued, got %d", http.StatusOK, rr.Code)
	}

	if createRunnerJobCalled {
		t.Error("CreateRunnerJob should not have been called when job is no longer queued")
	}
}

func TestHandleWebhook_K8sJobExistsError(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return false, errors.New("k8s connection error")
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d for k8s error, got %d", http.StatusInternalServerError, rr.Code)
	}
}

func TestHandleWebhook_GitHubIsJobQueuedError(t *testing.T) {
	ghClient := &mockGitHubClient{
		isJobQueuedFunc: func(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
			return false, errors.New("github api error")
		},
	}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return false, nil
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d for github api error, got %d", http.StatusInternalServerError, rr.Code)
	}
}

func TestHandleWebhook_GenerateJITConfigError(t *testing.T) {
	ghClient := &mockGitHubClient{
		isJobQueuedFunc: func(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
			return true, nil
		},
		generateJITConfigFunc: func(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error) {
			return nil, errors.New("failed to generate jit config")
		},
	}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return false, nil
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d for jit config error, got %d", http.StatusInternalServerError, rr.Code)
	}
}

func TestHandleWebhook_CreateRunnerJobError(t *testing.T) {
	ghClient := &mockGitHubClient{
		isJobQueuedFunc: func(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
			return true, nil
		},
		generateJITConfigFunc: func(ctx context.Context, owner, repo, runnerName string, labels []string) (*ghclient.JITConfig, error) {
			return &ghclient.JITConfig{EncodedJITConfig: "config", RunnerID: 1}, nil
		},
	}
	k8sClient := &mockK8sClient{
		jobExistsFunc: func(ctx context.Context, name string) (bool, error) {
			return false, nil
		},
		createRunnerJobFunc: func(ctx context.Context, config k8s.RunnerJobConfig) error {
			return errors.New("failed to create k8s job")
		},
	}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d for create job error, got %d", http.StatusInternalServerError, rr.Code)
	}
}

func TestHandleWebhook_MissingSignatureHeader(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"self-hosted"})

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d for missing signature, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestHandleWebhook_WithoutSelfHostedLabel(t *testing.T) {
	ghClient := &mockGitHubClient{}
	k8sClient := &mockK8sClient{}
	scaler := newTestScaler(ghClient, k8sClient)

	body := createWebhookPayload("queued", 12345, "testowner", "testrepo", []string{"linux", "large"})
	req := createSignedRequest(scaler.webhookSecret, body)
	rr := httptest.NewRecorder()
	scaler.HandleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status %d when self-hosted label missing, got %d", http.StatusOK, rr.Code)
	}
}
