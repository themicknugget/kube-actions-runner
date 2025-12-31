package scaler

import (
	"context"
	"fmt"
	"time"

	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/metrics"
)

// Reconciler periodically checks for queued GitHub Actions jobs
// and creates runner pods for any that don't have one
type Reconciler struct {
	ghClientFactory *ghclient.ClientFactory
	k8sClient       *k8s.Client
	scaler          *Scaler
	logger          *logger.Logger
	interval        time.Duration
	labelMatchers   []LabelMatcher
}

// ReconcilerConfig holds configuration for the Reconciler
type ReconcilerConfig struct {
	GHClientFactory *ghclient.ClientFactory
	K8sClient       *k8s.Client
	Scaler          *Scaler
	Logger          *logger.Logger
	Interval        time.Duration
	LabelMatchers   []LabelMatcher
}

// NewReconciler creates a new Reconciler
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	interval := cfg.Interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	return &Reconciler{
		ghClientFactory: cfg.GHClientFactory,
		k8sClient:       cfg.K8sClient,
		scaler:          cfg.Scaler,
		logger:          cfg.Logger,
		interval:        interval,
		labelMatchers:   cfg.LabelMatchers,
	}
}

// Start begins the reconciliation loop
func (r *Reconciler) Start(ctx context.Context) {
	log := r.logger.With("component", "reconciler")
	log.Info("starting reconciler", "interval", r.interval)

	// Run immediately on start
	r.reconcile(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("reconciler stopped")
			return
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) {
	log := r.logger.With("component", "reconciler")
	log.Info("starting reconciliation cycle")
	metrics.ReconcilerCyclesTotal.Inc()

	// Get all configured owners from the token registry
	registry := r.ghClientFactory.GetRegistry()
	owners := registry.GetConfiguredOwners()

	for _, owner := range owners {
		if err := r.reconcileOwner(ctx, owner); err != nil {
			log.Error("failed to reconcile owner", "owner", owner, "error", err)
		}
	}
	log.Info("reconciliation cycle complete", "owners_checked", len(owners))
}

func (r *Reconciler) reconcileOwner(ctx context.Context, owner string) error {
	log := r.logger.With("component", "reconciler", "owner", owner)
	log.Info("reconciling owner")

	ghClient, err := r.ghClientFactory.GetClientForOwner(owner)
	if err != nil {
		return fmt.Errorf("failed to get GitHub client: %w", err)
	}

	// List repositories for this owner
	repos, err := ghClient.ListRepositories(ctx, owner)
	if err != nil {
		return fmt.Errorf("failed to list repositories: %w", err)
	}

	// Log first few repos for debugging
	sampleRepos := repos
	if len(sampleRepos) > 5 {
		sampleRepos = repos[:5]
	}
	log.Info("found repositories", "count", len(repos), "sample", sampleRepos)

	for _, repo := range repos {
		if err := r.reconcileRepo(ctx, ghClient, owner, repo); err != nil {
			log.Error("failed to reconcile repo", "repo", repo, "error", err)
			// Continue with other repos
		}
	}

	return nil
}

func (r *Reconciler) reconcileRepo(ctx context.Context, ghClient *ghclient.Client, owner, repo string) error {
	log := r.logger.With("component", "reconciler", "owner", owner, "repo", repo)
	log.Debug("checking repo for queued jobs")

	// List queued jobs for this repo
	queuedJobs, err := ghClient.ListQueuedJobs(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("failed to list queued jobs: %w", err)
	}

	if len(queuedJobs) == 0 {
		log.Debug("no queued jobs found")
		return nil
	}

	log.Info("found queued jobs", "count", len(queuedJobs))

	// Get existing runner pods
	existingPods, err := r.k8sClient.ListRunnerPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to list runner pods: %w", err)
	}

	// Create a set of existing job IDs
	existingJobIDs := make(map[int64]bool)
	for _, pod := range existingPods {
		if jobID, ok := pod.Labels["job-id"]; ok {
			var id int64
			fmt.Sscanf(jobID, "%d", &id)
			existingJobIDs[id] = true
		}
	}

	for _, job := range queuedJobs {
		// Check if job matches our label matchers
		if !r.matchesLabels(job.Labels) {
			continue
		}

		// Check if we already have a runner for this job
		if existingJobIDs[job.ID] {
			log.Info("runner already exists for job", "job_id", job.ID)
			continue
		}

		log.Info("creating runner for orphaned queued job", "job_id", job.ID, "job_name", job.Name)

		// Create runner for this job
		if err := r.createRunnerForJob(ctx, ghClient, job); err != nil {
			log.Error("failed to create runner for job", "job_id", job.ID, "error", err)
			metrics.ReconcilerJobsFailedTotal.WithLabelValues(owner, repo).Inc()
			continue
		}

		metrics.ReconcilerJobsCreatedTotal.WithLabelValues(owner, repo).Inc()
	}

	return nil
}

func (r *Reconciler) matchesLabels(labels []string) bool {
	return ShouldHandle(labels, r.labelMatchers)
}

func (r *Reconciler) createRunnerForJob(ctx context.Context, ghClient *ghclient.Client, job ghclient.QueuedJob) error {
	jobName := fmt.Sprintf("runner-%d", job.ID)

	// Generate JIT config
	jitConfig, err := ghClient.GenerateJITConfig(ctx, job.Owner, job.Repo, jobName, job.Labels)
	if err != nil {
		return fmt.Errorf("failed to generate JIT config: %w", err)
	}

	// Use the scaler's createRunnerJob to maintain rate limiting
	return r.scaler.createRunnerJob(ctx, jobName, jitConfig.EncodedJITConfig, job.Owner, job.Repo, job.ID, job.Labels)
}
