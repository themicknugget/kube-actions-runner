package github

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/metrics"
)

type Client struct {
	client        *github.Client
	runnerGroupID int64
}

func NewClient(token string, runnerGroupID int64) *Client {
	if runnerGroupID == 0 {
		runnerGroupID = 1
	}
	return &Client{
		client:        github.NewClient(nil).WithAuthToken(token),
		runnerGroupID: runnerGroupID,
	}
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
