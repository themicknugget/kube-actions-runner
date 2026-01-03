package scaler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/metrics"
)

// rateLimitThreshold is the minimum rate limit remaining before skipping reconciliation
const rateLimitThreshold = 100

// Reconciler periodically checks for queued GitHub Actions jobs
// and creates runner pods for any that don't have one
type Reconciler struct {
	ghClientFactory  *ghclient.ClientFactory
	k8sClient        *k8s.Client
	scaler           *Scaler
	logger           *logger.Logger
	interval         time.Duration // Normal interval (default 5 minutes)
	activeInterval   time.Duration // Active interval when jobs are queued (default 60 seconds)
	inactivityWindow time.Duration // Time before returning to normal interval (default 10 minutes)
	labelMatchers    []LabelMatcher
	// reposToCheck is a list of owner/repo pairs to check for queued jobs
	// This is populated by webhook discovery. If empty, reconciliation is skipped.
	reposToCheck []string

	// Activity tracking for adaptive intervals
	mu               sync.Mutex
	lastActivityTime time.Time
	isActiveMode     bool

	// activityCh is used to notify the reconciler loop when activity is recorded
	// This allows immediate transition to active mode without waiting for the ticker
	activityCh chan struct{}
}

// ReconcilerConfig holds configuration for the Reconciler
type ReconcilerConfig struct {
	GHClientFactory  *ghclient.ClientFactory
	K8sClient        *k8s.Client
	Scaler           *Scaler
	Logger           *logger.Logger
	Interval         time.Duration // Normal interval (default 5 minutes)
	ActiveInterval   time.Duration // Active interval when jobs are queued (default 60 seconds)
	InactivityWindow time.Duration // Time before returning to normal interval (default 10 minutes)
	LabelMatchers    []LabelMatcher
	// ReposToCheck is a list of owner/repo pairs to check (from webhook discovery)
	// If empty, reconciliation will be skipped until discovery completes.
	ReposToCheck []string
}

// NewReconciler creates a new Reconciler
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	interval := cfg.Interval
	if interval == 0 {
		// Default to 5 minutes to avoid rate limiting
		interval = 5 * time.Minute
	}

	activeInterval := cfg.ActiveInterval
	if activeInterval == 0 {
		activeInterval = 60 * time.Second
	}

	inactivityWindow := cfg.InactivityWindow
	if inactivityWindow == 0 {
		inactivityWindow = 10 * time.Minute
	}

	return &Reconciler{
		ghClientFactory:  cfg.GHClientFactory,
		k8sClient:        cfg.K8sClient,
		scaler:           cfg.Scaler,
		logger:           cfg.Logger,
		interval:         interval,
		activeInterval:   activeInterval,
		inactivityWindow: inactivityWindow,
		labelMatchers:    cfg.LabelMatchers,
		reposToCheck:     cfg.ReposToCheck,
		activityCh:       make(chan struct{}, 1), // Buffered to avoid blocking
	}
}

// SetReposToCheck updates the list of repos to check (called after discovery)
func (r *Reconciler) SetReposToCheck(repos []string) {
	r.reposToCheck = repos
	r.logger.Info("reconciler repos updated", "count", len(repos))
}

// Start begins the reconciliation loop with adaptive intervals
func (r *Reconciler) Start(ctx context.Context) {
	log := r.logger.With("component", "reconciler")
	log.Info("starting reconciler",
		"interval", r.interval,
		"active_interval", r.activeInterval,
		"inactivity_window", r.inactivityWindow,
	)

	// Run immediately on start
	r.reconcile(ctx)

	// Start with normal interval
	currentInterval := r.interval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("reconciler stopped")
			return
		case <-r.activityCh:
			// Activity was recorded - switch to active interval immediately
			newInterval := r.calculateInterval()
			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(currentInterval)
			}
			// Run reconcile immediately on activity
			r.reconcile(ctx)
		case <-ticker.C:
			r.reconcile(ctx)

			// Check if we need to adjust the interval
			newInterval := r.calculateInterval()
			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(currentInterval)
			}
		}
	}
}

// RecordActivity marks that workflow activity was detected.
// This is called by both the reconciler (when finding queued jobs) and
// the webhook handler (when receiving workflow events) to trigger
// more frequent polling.
func (r *Reconciler) RecordActivity() {
	r.mu.Lock()
	r.lastActivityTime = time.Now()
	r.mu.Unlock()

	// Non-blocking send to notify the reconciler loop
	// If the channel is full, the reconciler will pick up the activity on next tick
	select {
	case r.activityCh <- struct{}{}:
	default:
	}
}

// isRateLimitLow checks if any owner has a rate limit below the threshold.
// Returns the owner name and remaining limit if low, or empty string and -1 if OK.
func (r *Reconciler) isRateLimitLow() (string, int) {
	registry := r.ghClientFactory.GetRegistry()
	owners := registry.GetConfiguredOwners()

	for _, owner := range owners {
		remaining := ghclient.GetRateLimitRemaining(owner)
		// Skip if we don't have rate limit info yet (returns -1)
		if remaining >= 0 && remaining < rateLimitThreshold {
			return owner, remaining
		}
	}
	return "", -1
}

// calculateInterval determines the appropriate interval based on recent activity
func (r *Reconciler) calculateInterval() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	// If we've never seen activity, use normal interval
	if r.lastActivityTime.IsZero() {
		if r.isActiveMode {
			r.isActiveMode = false
			r.logger.Info("reconciler returning to normal interval (no activity recorded)",
				"interval", r.interval,
			)
		}
		return r.interval
	}

	// Check if we're within the inactivity window
	timeSinceActivity := time.Since(r.lastActivityTime)
	if timeSinceActivity < r.inactivityWindow {
		// Still within active window - use fast interval
		if !r.isActiveMode {
			r.isActiveMode = true
			r.logger.Info("reconciler entering active mode",
				"time_since_activity", timeSinceActivity.Round(time.Second),
				"interval", r.activeInterval,
			)
		}
		return r.activeInterval
	}

	// Past inactivity window - return to normal interval
	if r.isActiveMode {
		r.isActiveMode = false
		r.logger.Info("reconciler returning to normal interval",
			"time_since_activity", timeSinceActivity.Round(time.Second),
			"interval", r.interval,
		)
	}
	return r.interval
}

func (r *Reconciler) reconcile(ctx context.Context) {
	log := r.logger.With("component", "reconciler")
	log.Debug("starting reconciliation cycle")
	metrics.ReconcilerCyclesTotal.Inc()

	// Check rate limit before making API calls
	if owner, remaining := r.isRateLimitLow(); owner != "" {
		log.Warn("skipping reconciliation cycle due to low rate limit",
			"owner", owner,
			"remaining", remaining,
			"threshold", rateLimitThreshold,
		)
		return
	}

	// Sync the active jobs gauge to ensure it reflects actual state
	// This provides self-healing if the watcher misses events
	if err := r.k8sClient.SyncActiveJobsMetric(ctx); err != nil {
		log.Error("failed to sync active jobs metric", "error", err.Error())
	}

	// Clean up orphaned runners (runners whose GitHub job is no longer queued)
	r.cleanupOrphanedRunners(ctx)

	// Only check repos that were discovered via webhook sync
	// If no repos are configured, skip this cycle (waiting for discovery to complete)
	if len(r.reposToCheck) == 0 {
		log.Debug("no repos configured for reconciliation, waiting for webhook discovery")
		return
	}

	for _, fullRepo := range r.reposToCheck {
		parts := strings.SplitN(fullRepo, "/", 2)
		if len(parts) != 2 {
			continue
		}
		owner, repo := parts[0], parts[1]

		ghClient, err := r.ghClientFactory.GetClientForOwner(owner)
		if err != nil {
			log.Error("failed to get GitHub client", "owner", owner, "error", err.Error())
			continue
		}

		if err := r.reconcileRepo(ctx, ghClient, owner, repo); err != nil {
			log.Error("failed to reconcile repo", "repo", fullRepo, "error", err.Error())
		}
	}
}

// cleanupOrphanedRunners checks all active runner jobs and removes runners
// whose corresponding GitHub workflow job is no longer queued (completed, cancelled, or taken).
// Instead of force-deleting pods, it unregisters the runner from GitHub which causes
// the runner process to exit gracefully.
func (r *Reconciler) cleanupOrphanedRunners(ctx context.Context) {
	log := r.logger.With("component", "reconciler", "operation", "cleanup")

	// List all active runner jobs in K8s
	activeJobs, err := r.k8sClient.ListActiveRunnerJobs(ctx)
	if err != nil {
		log.Error("failed to list active runner jobs", "error", err.Error())
		return
	}

	if len(activeJobs) == 0 {
		return
	}

	log.Debug("checking for orphaned runners", "active_jobs", len(activeJobs))

	for _, job := range activeJobs {
		// Skip jobs without required metadata
		if job.Owner == "" || job.Repo == "" || job.JobID == 0 {
			continue
		}

		// Skip jobs where cleanup was already attempted (prevents retry storms)
		if job.CleanupAttempted {
			log.Debug("skipping job with previous cleanup attempt",
				"runner", job.RunnerName,
				"owner", job.Owner,
				"repo", job.Repo)
			continue
		}

		// Only check jobs whose pods are Running (they've had time to register)
		// Skip Pending pods as they may still be starting up
		if job.Phase != corev1.PodRunning {
			continue
		}

		// Get GitHub client for this owner
		ghClient, err := r.ghClientFactory.GetClientForOwner(job.Owner)
		if err != nil {
			log.Debug("no GitHub client for owner", "owner", job.Owner)
			continue
		}

		// Check the status of the GitHub workflow job
		status, err := ghClient.GetWorkflowJobStatus(ctx, job.Owner, job.Repo, job.JobID)
		if err != nil {
			log.Error("failed to get workflow job status",
				"owner", job.Owner,
				"repo", job.Repo,
				"job_id", job.JobID,
				"error", err)
			metrics.OrphanedRunnerCleanupErrorsTotal.WithLabelValues(job.Owner, job.Repo).Inc()
			continue
		}

		// Determine if the runner is orphaned
		var cleanupReason string

		if status == nil {
			// Job not found (404) - it was deleted
			cleanupReason = "job_not_found"
		} else if status.Status == "completed" {
			// Job completed (success, failure, cancelled, etc.)
			cleanupReason = fmt.Sprintf("completed_%s", status.Conclusion)
		} else if status.Status == "in_progress" {
			// Job is being worked on - this is normal, the runner is running the job
			// No cleanup needed
			continue
		} else if status.Status == "queued" {
			// Job is still queued - runner is waiting for work, this is normal
			continue
		}

		if cleanupReason == "" {
			continue
		}

		// This runner is orphaned - unregister it from GitHub
		log.Info("cleaning up orphaned runner",
			"runner", job.RunnerName,
			"owner", job.Owner,
			"repo", job.Repo,
			"job_id", job.JobID,
			"reason", cleanupReason,
			"job_status", status)

		// Delete the runner from GitHub - this causes the runner process to exit
		// with the message: "The runner registration has been deleted from the server"
		deleted, err := ghClient.DeleteRunnerByName(ctx, job.Owner, job.Repo, job.RunnerName)
		if err != nil {
			log.Error("failed to delete orphaned runner",
				"runner", job.RunnerName,
				"error", err.Error())
			metrics.OrphanedRunnerCleanupErrorsTotal.WithLabelValues(job.Owner, job.Repo).Inc()
			// Mark cleanup as attempted to prevent retry storms
			if markErr := r.k8sClient.MarkCleanupAttempted(ctx, job.Name); markErr != nil {
				log.Error("failed to mark cleanup attempted",
					"job", job.Name,
					"error", markErr)
			}
			continue
		}

		if deleted {
			log.Info("successfully unregistered orphaned runner",
				"runner", job.RunnerName,
				"owner", job.Owner,
				"repo", job.Repo)
			metrics.OrphanedRunnersCleanedTotal.WithLabelValues(job.Owner, job.Repo, cleanupReason).Inc()
		} else {
			// Runner wasn't found in GitHub - it may have already been removed
			// Mark cleanup as attempted to prevent retry storms on subsequent cycles
			log.Info("orphaned runner not found in GitHub (already removed), marking cleanup attempted",
				"runner", job.RunnerName)
			if markErr := r.k8sClient.MarkCleanupAttempted(ctx, job.Name); markErr != nil {
				log.Error("failed to mark cleanup attempted",
					"job", job.Name,
					"error", markErr)
			}
		}
	}
}

func (r *Reconciler) reconcileRepo(ctx context.Context, ghClient *ghclient.Client, owner, repo string) error {
	log := r.logger.With("component", "reconciler", "owner", owner, "repo", repo)

	// List queued jobs for this repo
	queuedJobs, err := ghClient.ListQueuedJobs(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("failed to list queued jobs: %w", err)
	}

	if len(queuedJobs) == 0 {
		return nil
	}

	// Record activity - queued jobs found means we should poll more frequently
	r.RecordActivity()

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
			continue
		}

		log.Info("creating runner for queued job", "job_id", job.ID, "job_name", job.Name)

		// Create runner for this job
		if err := r.createRunnerForJob(ctx, ghClient, job); err != nil {
			log.Error("failed to create runner for job", "job_id", job.ID, "error", err.Error())
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
	log := r.logger.With("component", "reconciler", "owner", job.Owner, "repo", job.Repo, "job_id", job.ID)
	runnerName := fmt.Sprintf("runner-%d", job.ID)

	// Generate JIT config
	jitConfig, err := ghClient.GenerateJITConfig(ctx, job.Owner, job.Repo, runnerName, job.Labels)
	if err != nil {
		// Check if it's a 409 conflict (stale runner exists)
		if strings.Contains(err.Error(), "409") && strings.Contains(err.Error(), "Already exists") {
			log.Info("stale runner found, deleting", "runner_name", runnerName)
			deleted, delErr := ghClient.DeleteRunnerByName(ctx, job.Owner, job.Repo, runnerName)
			if delErr != nil {
				return fmt.Errorf("failed to delete stale runner: %w", delErr)
			}
			if deleted {
				log.Info("deleted stale runner, retrying JIT config generation")
				// Retry JIT config generation
				jitConfig, err = ghClient.GenerateJITConfig(ctx, job.Owner, job.Repo, runnerName, job.Labels)
				if err != nil {
					return fmt.Errorf("failed to generate JIT config after cleanup: %w", err)
				}
			} else {
				return fmt.Errorf("stale runner not found for deletion: %w", err)
			}
		} else {
			return fmt.Errorf("failed to generate JIT config: %w", err)
		}
	}

	// Use the scaler's createRunnerJob to maintain rate limiting
	_, err = r.scaler.createRunnerJob(ctx, runnerName, jitConfig.EncodedJITConfig, job.Owner, job.Repo, job.ID, job.Labels)
	return err
}
