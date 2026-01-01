package scaler

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	interval        time.Duration // Normal interval (default 5 minutes)
	activeInterval  time.Duration // Active interval when jobs are queued (default 30 seconds)
	inactivityWindow time.Duration // Time before returning to normal interval (default 10 minutes)
	labelMatchers   []LabelMatcher
	// reposToCheck is a list of owner/repo pairs to check for queued jobs
	// If empty, all repos will be checked (expensive)
	reposToCheck []string

	// Activity tracking for adaptive intervals
	mu               sync.Mutex
	lastActivityTime time.Time
	isActiveMode     bool
}

// ReconcilerConfig holds configuration for the Reconciler
type ReconcilerConfig struct {
	GHClientFactory  *ghclient.ClientFactory
	K8sClient        *k8s.Client
	Scaler           *Scaler
	Logger           *logger.Logger
	Interval         time.Duration // Normal interval (default 5 minutes)
	ActiveInterval   time.Duration // Active interval when jobs are queued (default 30 seconds)
	InactivityWindow time.Duration // Time before returning to normal interval (default 10 minutes)
	LabelMatchers    []LabelMatcher
	// ReposToCheck is a list of owner/repo pairs to check (from discovery)
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
		activeInterval = 30 * time.Second
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

// recordActivity marks that workflow activity was detected
func (r *Reconciler) recordActivity() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastActivityTime = time.Now()
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

	// Sync the active jobs gauge to ensure it reflects actual state
	// This provides self-healing if the watcher misses events
	if err := r.k8sClient.SyncActiveJobsMetric(ctx); err != nil {
		log.Error("failed to sync active jobs metric", "error", err)
	}

	// If we have a specific list of repos to check, use that
	if len(r.reposToCheck) > 0 {
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
		return
	}

	// Fallback: Get all configured owners and check all repos (expensive)
	registry := r.ghClientFactory.GetRegistry()
	owners := registry.GetConfiguredOwners()

	for _, owner := range owners {
		if err := r.reconcileOwner(ctx, owner); err != nil {
			log.Error("failed to reconcile owner", "owner", owner, "error", err.Error())
		}
	}
}

func (r *Reconciler) reconcileOwner(ctx context.Context, owner string) error {
	log := r.logger.With("component", "reconciler", "owner", owner)

	ghClient, err := r.ghClientFactory.GetClientForOwner(owner)
	if err != nil {
		return fmt.Errorf("failed to get GitHub client: %w", err)
	}

	// List repositories for this owner
	repos, err := ghClient.ListRepositories(ctx, owner)
	if err != nil {
		return fmt.Errorf("failed to list repositories: %w", err)
	}

	for _, repo := range repos {
		if err := r.reconcileRepo(ctx, ghClient, owner, repo); err != nil {
			log.Error("failed to reconcile repo", "repo", repo, "error", err.Error())
		}
	}

	return nil
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
	r.recordActivity()

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
	return r.scaler.createRunnerJob(ctx, runnerName, jitConfig.EncodedJITConfig, job.Owner, job.Repo, job.ID, job.Labels)
}
