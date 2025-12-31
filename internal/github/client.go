package github

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/metrics"
	"github.com/kube-actions-runner/kube-actions-runner/internal/tokens"
)

type Client struct {
	client        *github.Client
	runnerGroupID int64
	token         string
}

// NewClient creates a new GitHub client with the given token and runner group ID
func NewClient(token string, runnerGroupID int64) *Client {
	if runnerGroupID == 0 {
		runnerGroupID = 1
	}
	return &Client{
		client:        github.NewClient(nil).WithAuthToken(token),
		runnerGroupID: runnerGroupID,
		token:         token,
	}
}

// ClientFactory creates GitHub clients for specific owners using a token registry
type ClientFactory struct {
	registry      *tokens.Registry
	runnerGroupID int64
}

// NewClientFactory creates a new client factory with the given token registry
func NewClientFactory(registry *tokens.Registry, runnerGroupID int64) *ClientFactory {
	if runnerGroupID == 0 {
		runnerGroupID = 1
	}
	return &ClientFactory{
		registry:      registry,
		runnerGroupID: runnerGroupID,
	}
}

// GetClientForOwner returns a GitHub client configured with the appropriate token for the owner
func (f *ClientFactory) GetClientForOwner(owner string) (*Client, error) {
	token, err := f.registry.GetTokenForOwner(owner)
	if err != nil {
		return nil, fmt.Errorf("failed to get token for owner %s: %w", owner, err)
	}
	return NewClient(token, f.runnerGroupID), nil
}

// GetRegistry returns the underlying token registry
func (f *ClientFactory) GetRegistry() *tokens.Registry {
	return f.registry
}

func (c *Client) recordAPIMetrics(endpoint string, startTime time.Time, resp *github.Response, err error) {
	metrics.GitHubAPILatencySeconds.WithLabelValues(endpoint).Observe(time.Since(startTime).Seconds())

	if err != nil {
		metrics.GitHubAPIRequestsTotal.WithLabelValues(endpoint, "error").Inc()
		return
	}

	metrics.GitHubAPIRequestsTotal.WithLabelValues(endpoint, metrics.StatusCategory(resp.StatusCode)).Inc()

	if resp.Rate.Remaining >= 0 {
		metrics.GitHubAPIRateLimitRemaining.Set(float64(resp.Rate.Remaining))
	}
}

type JITConfig struct {
	EncodedJITConfig string
	RunnerID         int64
}

func (c *Client) GenerateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string) (*JITConfig, error) {
	const endpoint = "generate_jit_config"
	startTime := time.Now()

	req := &github.GenerateJITConfigRequest{
		Name:          runnerName,
		RunnerGroupID: c.runnerGroupID,
		Labels:        labels,
	}

	config, resp, err := c.client.Actions.GenerateRepoJITConfig(ctx, owner, repo, req)
	c.recordAPIMetrics(endpoint, startTime, resp, err)

	if err != nil {
		return nil, fmt.Errorf("failed to generate JIT config: %w", err)
	}
	defer resp.Body.Close()

	return &JITConfig{
		EncodedJITConfig: config.GetEncodedJITConfig(),
		RunnerID:         config.GetRunner().GetID(),
	}, nil
}

func (c *Client) IsJobQueued(ctx context.Context, owner, repo string, jobID int64) (bool, error) {
	const endpoint = "get_job"
	startTime := time.Now()

	job, resp, err := c.client.Actions.GetWorkflowJobByID(ctx, owner, repo, jobID)
	c.recordAPIMetrics(endpoint, startTime, resp, err)

	if err != nil {
		return false, fmt.Errorf("failed to get workflow job: %w", err)
	}
	defer resp.Body.Close()

	return job.GetStatus() == "queued", nil
}

// QueuedJob represents a queued workflow job
type QueuedJob struct {
	ID       int64
	Name     string
	Owner    string
	Repo     string
	Labels   []string
	RunID    int64
}

// ListRepositories lists all repositories for an owner (user or org)
func (c *Client) ListRepositories(ctx context.Context, owner string) ([]string, error) {
	const endpoint = "list_repos"
	var repos []string

	// Try as org first
	opts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		startTime := time.Now()
		repoList, resp, err := c.client.Repositories.ListByOrg(ctx, owner, opts)
		c.recordAPIMetrics(endpoint, startTime, resp, err)

		if err != nil {
			// If not an org, try as user
			if resp != nil && resp.StatusCode == 404 {
				return c.listUserRepositories(ctx, owner)
			}
			return nil, fmt.Errorf("failed to list org repositories: %w", err)
		}
		defer resp.Body.Close()

		for _, repo := range repoList {
			repos = append(repos, repo.GetName())
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return repos, nil
}

func (c *Client) listUserRepositories(ctx context.Context, owner string) ([]string, error) {
	const endpoint = "list_user_repos"
	var repos []string

	// Use ListByAuthenticatedUser to get all repos the user has access to
	// including collaborator and organization member repos
	opts := &github.RepositoryListByAuthenticatedUserOptions{
		Affiliation: "owner,collaborator,organization_member",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		startTime := time.Now()
		repoList, resp, err := c.client.Repositories.ListByAuthenticatedUser(ctx, opts)
		c.recordAPIMetrics(endpoint, startTime, resp, err)

		if err != nil {
			return nil, fmt.Errorf("failed to list user repositories: %w", err)
		}
		defer resp.Body.Close()

		for _, repo := range repoList {
			repos = append(repos, repo.GetName())
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return repos, nil
}

// ListQueuedJobs lists all queued workflow jobs for a repository
func (c *Client) ListQueuedJobs(ctx context.Context, owner, repo string) ([]QueuedJob, error) {
	const endpoint = "list_workflow_runs"
	var queuedJobs []QueuedJob

	// List workflow runs that are queued or in_progress (to catch queued jobs within)
	opts := &github.ListWorkflowRunsOptions{
		Status:      "queued",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	startTime := time.Now()
	runs, resp, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
	c.recordAPIMetrics(endpoint, startTime, resp, err)

	if err != nil {
		return nil, fmt.Errorf("failed to list workflow runs: %w", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	// For each run, get the jobs
	for _, run := range runs.WorkflowRuns {
		jobs, err := c.listJobsForRun(ctx, owner, repo, run.GetID())
		if err != nil {
			continue // Skip runs we can't get jobs for
		}
		queuedJobs = append(queuedJobs, jobs...)
	}

	// Also check in_progress runs which may have queued jobs
	opts.Status = "in_progress"
	startTime = time.Now()
	runs, resp, err = c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
	c.recordAPIMetrics(endpoint, startTime, resp, err)

	if err == nil {
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}
		for _, run := range runs.WorkflowRuns {
			jobs, err := c.listJobsForRun(ctx, owner, repo, run.GetID())
			if err != nil {
				continue
			}
			queuedJobs = append(queuedJobs, jobs...)
		}
	}

	return queuedJobs, nil
}

func (c *Client) listJobsForRun(ctx context.Context, owner, repo string, runID int64) ([]QueuedJob, error) {
	const endpoint = "list_jobs_for_run"
	var queuedJobs []QueuedJob

	opts := &github.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	startTime := time.Now()
	jobs, resp, err := c.client.Actions.ListWorkflowJobs(ctx, owner, repo, runID, opts)
	c.recordAPIMetrics(endpoint, startTime, resp, err)

	if err != nil {
		return nil, fmt.Errorf("failed to list workflow jobs: %w", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	for _, job := range jobs.Jobs {
		if job.GetStatus() == "queued" {
			var labels []string
			for _, label := range job.Labels {
				labels = append(labels, label)
			}
			queuedJobs = append(queuedJobs, QueuedJob{
				ID:     job.GetID(),
				Name:   job.GetName(),
				Owner:  owner,
				Repo:   repo,
				Labels: labels,
				RunID:  runID,
			})
		}
	}

	return queuedJobs, nil
}

// DeleteRunnerByName finds and deletes a runner by name
// Returns true if a runner was deleted, false if not found
func (c *Client) DeleteRunnerByName(ctx context.Context, owner, repo, runnerName string) (bool, error) {
	const endpoint = "list_runners"

	// List all runners for the repo
	opts := &github.ListOptions{PerPage: 100}

	for {
		startTime := time.Now()
		runners, resp, err := c.client.Actions.ListRunners(ctx, owner, repo, opts)
		c.recordAPIMetrics(endpoint, startTime, resp, err)

		if err != nil {
			return false, fmt.Errorf("failed to list runners: %w", err)
		}
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}

		// Find and delete the runner with matching name
		for _, runner := range runners.Runners {
			if runner.GetName() == runnerName {
				startTime = time.Now()
				delResp, err := c.client.Actions.RemoveRunner(ctx, owner, repo, runner.GetID())
				c.recordAPIMetrics("delete_runner", startTime, delResp, err)

				if err != nil {
					return false, fmt.Errorf("failed to delete runner %s: %w", runnerName, err)
				}
				if delResp != nil && delResp.Body != nil {
					defer delResp.Body.Close()
				}
				return true, nil
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return false, nil
}
