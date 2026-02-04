package scaler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-github/v57/github"
	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/metrics"
	corev1 "k8s.io/api/core/v1"
)

type Scaler struct {
	webhookSecret    []byte
	labelMatchers    []LabelMatcher
	ghClientFactory  *ghclient.ClientFactory
	k8sClient        *k8s.Client
	logger           *logger.Logger
	runnerMode       k8s.RunnerMode
	runnerImage      string
	dindImage        string
	registryMirror   string
	cachePVC         string
	ttlSeconds       int32
	skipNodeCheck    bool
	tolerations      []corev1.Toleration
	jobCreateMu      sync.Mutex // Serializes job creation for better topology spread
	reconciler       *Reconciler // For recording activity to trigger faster polling
}

type Config struct {
	WebhookSecret   []byte
	LabelMatchers   []LabelMatcher
	GHClientFactory *ghclient.ClientFactory
	K8sClient       *k8s.Client
	Logger          *logger.Logger
	RunnerMode      k8s.RunnerMode
	RunnerImage     string
	DindImage       string
	RegistryMirror  string
	CachePVC        string
	TTLSeconds      int32
	SkipNodeCheck   bool
	Tolerations     []corev1.Toleration
}

func NewScaler(cfg Config) *Scaler {
	return &Scaler{
		webhookSecret:   cfg.WebhookSecret,
		labelMatchers:   cfg.LabelMatchers,
		ghClientFactory: cfg.GHClientFactory,
		k8sClient:       cfg.K8sClient,
		logger:          cfg.Logger,
		runnerMode:      cfg.RunnerMode,
		runnerImage:     cfg.RunnerImage,
		dindImage:       cfg.DindImage,
		registryMirror:  cfg.RegistryMirror,
		cachePVC:        cfg.CachePVC,
		ttlSeconds:      cfg.TTLSeconds,
		skipNodeCheck:   cfg.SkipNodeCheck,
		tolerations:     cfg.Tolerations,
	}
}

// SetReconciler sets the reconciler reference for activity recording.
// This allows webhook events to trigger faster polling in the reconciler.
func (s *Scaler) SetReconciler(r *Reconciler) {
	s.reconciler = r
}

// recordActivity notifies the reconciler that workflow activity occurred
func (s *Scaler) recordActivity() {
	if s.reconciler != nil {
		s.reconciler.RecordActivity()
	}
}

func (s *Scaler) validateRequest(r *http.Request) error {
	if !ValidateSignature(s.webhookSecret, r) {
		return fmt.Errorf("invalid signature")
	}
	return nil
}

func (s *Scaler) parseWorkflowEvent(r *http.Request) (*github.WorkflowJobEvent, error) {
	var event github.WorkflowJobEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		return nil, fmt.Errorf("failed to decode payload: %w", err)
	}
	return &event, nil
}

func (s *Scaler) shouldProcessEvent(event *github.WorkflowJobEvent) (bool, string) {
	action := event.GetAction()
	if action != "queued" {
		return false, "non-queued event"
	}

	jobLabels := event.GetWorkflowJob().Labels
	if !ShouldHandle(jobLabels, s.labelMatchers) {
		return false, "labels do not match"
	}

	return true, ""
}

func (s *Scaler) processQueuedJob(ctx context.Context, w http.ResponseWriter, event *github.WorkflowJobEvent, respond func(int), respondError func(int, string)) {
	// Record activity to trigger faster reconciler polling
	s.recordActivity()

	owner := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()
	jobID := event.GetWorkflowJob().GetID()
	jobLabels := event.GetWorkflowJob().Labels

	log := s.logger.With(
		"owner", owner,
		"repo", repo,
		"job_id", jobID,
		"action", event.GetAction(),
	)

	log.Info("processing workflow job", "labels", jobLabels)

	// Get GitHub client for this owner
	ghClient, err := s.ghClientFactory.GetClientForOwner(owner)
	if err != nil {
		log.Error("no token configured for owner", "error", err.Error())
		// Return 200 to prevent GitHub from retrying - we can't handle this owner
		respond(http.StatusOK)
		return
	}

	jobName := fmt.Sprintf("runner-%d", jobID)

	exists, err := s.k8sClient.JobExists(ctx, jobName)
	if err != nil {
		log.Error("failed to check job existence", "error", err.Error())
		respondError(http.StatusInternalServerError, "internal error")
		return
	}
	if exists {
		log.Info("job already exists, skipping")
		respond(http.StatusOK)
		return
	}

	stillQueued, err := ghClient.IsJobQueued(ctx, owner, repo, jobID)
	if err != nil {
		log.Error("failed to verify job status", "error", err.Error())
		respondError(http.StatusInternalServerError, "failed to verify job status")
		return
	}
	if !stillQueued {
		log.Info("job no longer queued, skipping")
		respond(http.StatusOK)
		return
	}

	// Check node availability for required architecture
	if !s.skipNodeCheck {
		requiredArch := k8s.GetRequiredArchFromLabels(jobLabels)
		if requiredArch != "" {
			hasNodes, err := s.k8sClient.HasNodesForArchitecture(ctx, requiredArch)
			if err != nil {
				log.Error("failed to check node availability", "error", err.Error(), "arch", requiredArch)
				respondError(http.StatusInternalServerError, "failed to check node availability")
				return
			}
			if !hasNodes {
				log.Warn("no nodes available for architecture, job will remain queued in GitHub", "arch", requiredArch)
				respond(http.StatusOK)
				return
			}
		}
	}

	runnerName := fmt.Sprintf("runner-%d", jobID)

	// Track job creation time (JIT config + K8s job creation)
	creationStart := time.Now()

	jitConfig, err := ghClient.GenerateJITConfig(ctx, owner, repo, runnerName, jobLabels)
	if err != nil {
		log.Error("failed to generate JIT config", "error", err.Error())
		respondError(http.StatusInternalServerError, "failed to generate JIT config")
		return
	}

	log.Info("generated JIT config", "runner_id", jitConfig.RunnerID)

	actualMode, err := s.createRunnerJob(ctx, jobName, jitConfig.EncodedJITConfig, owner, repo, jobID, jobLabels)
	if err != nil {
		log.Error("failed to create runner job", "error", err.Error())
		respondError(http.StatusInternalServerError, "failed to create job")
		return
	}

	metrics.RunnerJobCreationDurationSeconds.WithLabelValues(owner, repo).Observe(time.Since(creationStart).Seconds())
	metrics.RunnerJobsCreatedTotal.WithLabelValues(owner, repo, string(actualMode)).Inc()
	metrics.RunnerJobsActive.Inc()

	log.Info("created runner job", "job_name", jobName, "runner_mode", actualMode)
	respond(http.StatusOK)
}

func (s *Scaler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	startTime := time.Now()

	respond := func(statusCode int, owner, repo string) {
		metrics.WebhookLatencySeconds.WithLabelValues(owner, repo).Observe(time.Since(startTime).Seconds())
		metrics.WebhookRequestsTotal.WithLabelValues(metrics.StatusCategory(statusCode), owner, repo).Inc()
		w.WriteHeader(statusCode)
	}

	respondError := func(statusCode int, owner, repo, message string) {
		metrics.WebhookLatencySeconds.WithLabelValues(owner, repo).Observe(time.Since(startTime).Seconds())
		metrics.WebhookRequestsTotal.WithLabelValues(metrics.StatusCategory(statusCode), owner, repo).Inc()
		http.Error(w, message, statusCode)
	}

	if err := s.validateRequest(r); err != nil {
		s.logger.Warn("invalid webhook signature")
		respondError(http.StatusUnauthorized, "unknown", "unknown", "invalid signature")
		return
	}

	event, err := s.parseWorkflowEvent(r)
	if err != nil {
		s.logger.Error("failed to parse webhook payload", "error", err)
		respondError(http.StatusBadRequest, "unknown", "unknown", "invalid payload")
		return
	}

	owner := event.GetRepo().GetOwner().GetLogin()
	repo := event.GetRepo().GetName()

	shouldProcess, reason := s.shouldProcessEvent(event)
	if !shouldProcess {
		s.logger.With(
			"owner", owner,
			"repo", repo,
			"job_id", event.GetWorkflowJob().GetID(),
			"action", event.GetAction(),
		).Debug("skipping event", "reason", reason)
		respond(http.StatusOK, owner, repo)
		return
	}

	processRespond := func(statusCode int) {
		respond(statusCode, owner, repo)
	}
	processRespondError := func(statusCode int, message string) {
		respondError(statusCode, owner, repo, message)
	}
	s.processQueuedJob(ctx, w, event, processRespond, processRespondError)
}

func (s *Scaler) createRunnerJob(ctx context.Context, name, jitConfig, owner, repo string, workflowID int64, labels []string) (k8s.RunnerMode, error) {
	config := k8s.RunnerJobConfig{
		Name:           name,
		JITConfig:      jitConfig,
		Owner:          owner,
		Repo:           repo,
		WorkflowID:     workflowID,
		Labels:         labels,
		RunnerMode:     s.runnerMode,
		RunnerImage:    s.runnerImage,
		DindImage:      s.dindImage,
		RegistryMirror: s.registryMirror,
		CachePVC:       s.cachePVC,
		TTLSeconds:     s.ttlSeconds,
		Tolerations:    s.tolerations,
	}

	// Serialize job creation to allow scheduler to see previous pods
	// This helps TopologySpreadConstraints work correctly during burst scheduling
	s.jobCreateMu.Lock()
	defer s.jobCreateMu.Unlock()

	result, err := s.k8sClient.CreateRunnerJob(ctx, config)
	if err != nil {
		return "", err
	}

	// Small delay to let scheduler process the pod before next job is created
	time.Sleep(150 * time.Millisecond)
	return result.ActualMode, nil
}
