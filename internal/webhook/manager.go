package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/discovery"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/tokens"
)

// Manager handles webhook registration and management
type Manager struct {
	registry      *tokens.Registry
	logger        *logger.Logger
	webhookURL    string
	webhookSecret string
}

// Config holds configuration for the webhook manager
type Config struct {
	TokenRegistry *tokens.Registry
	WebhookURL    string
	WebhookSecret string // If empty, will be auto-generated
	Logger        *logger.Logger
}

// NewManager creates a new webhook manager
func NewManager(cfg Config) (*Manager, error) {
	secret := cfg.WebhookSecret
	if secret == "" {
		var err error
		secret, err = generateSecret()
		if err != nil {
			return nil, fmt.Errorf("failed to generate webhook secret: %w", err)
		}
		cfg.Logger.Info("generated webhook secret")
	}

	return &Manager{
		registry:      cfg.TokenRegistry,
		logger:        cfg.Logger,
		webhookURL:    cfg.WebhookURL,
		webhookSecret: secret,
	}, nil
}

// GetWebhookSecret returns the webhook secret (auto-generated or configured)
func (m *Manager) GetWebhookSecret() string {
	return m.webhookSecret
}

// generateSecret generates a cryptographically secure webhook secret
func generateSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// RegisterResult contains the result of a webhook registration attempt
type RegisterResult struct {
	Repo      string
	Created   bool
	Updated   bool
	Unchanged bool
	Error     error
}

// RegisterWebhooks registers webhooks for all discovered repositories.
// If forceUpdate is true, webhooks are always updated regardless of current config.
func (m *Manager) RegisterWebhooks(ctx context.Context, repos []discovery.RepoInfo, forceUpdate bool) []RegisterResult {
	var results []RegisterResult

	for _, repo := range repos {
		// Use the token from RepoInfo if available, otherwise lookup from registry
		token := repo.Token
		if token == "" {
			var err error
			token, err = m.registry.GetTokenForOwner(repo.Owner)
			if err != nil {
				results = append(results, RegisterResult{
					Repo:  repo.FullName,
					Error: fmt.Errorf("no token configured for owner %s: %w", repo.Owner, err),
				})
				m.logger.Error("no token configured for owner", "repo", repo.FullName, "owner", repo.Owner)
				continue
			}
		}

		result := m.registerWebhookWithToken(ctx, repo.Owner, repo.Name, token, forceUpdate)
		results = append(results, result)

		// Rate limit protection
		time.Sleep(200 * time.Millisecond)
	}

	return results
}

// registerWebhookWithToken registers or updates a webhook for a single repository using the provided token.
// If forceUpdate is true, existing webhooks are always updated (used when secret changes).
func (m *Manager) registerWebhookWithToken(ctx context.Context, owner, repo, token string, forceUpdate bool) RegisterResult {
	fullName := fmt.Sprintf("%s/%s", owner, repo)
	result := RegisterResult{Repo: fullName}

	client := github.NewClient(nil).WithAuthToken(token)

	// List existing webhooks
	hooks, resp, err := client.Repositories.ListHooks(ctx, owner, repo, nil)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case 401:
				result.Error = fmt.Errorf("GitHub token is invalid or expired")
				m.logger.Error("GitHub token is invalid or expired", "repo", fullName)
			case 403:
				result.Error = fmt.Errorf("GitHub token lacks webhook read permission (fine-grained PAT needs 'Webhooks: Read and write' on this repo)")
				m.logger.Error("insufficient permissions to list webhooks",
					"repo", fullName,
					"hint", "fine-grained PAT needs 'Webhooks: Read and write' permission")
			case 404:
				result.Error = fmt.Errorf("repository not found or no access (token may lack 'repo' scope)")
				m.logger.Error("repository not found or no access", "repo", fullName)
			default:
				result.Error = fmt.Errorf("failed to list webhooks: %w", err)
				m.logger.Error("failed to list webhooks", "repo", fullName, "error", err)
			}
		} else {
			result.Error = fmt.Errorf("failed to list webhooks: %w", err)
			m.logger.Error("failed to list webhooks", "repo", fullName, "error", err)
		}
		return result
	}

	// Check if our webhook already exists
	var existingHook *github.Hook
	for _, hook := range hooks {
		if hook.Config != nil {
			if url, ok := hook.Config["url"].(string); ok {
				if url == m.webhookURL {
					existingHook = hook
					break
				}
			}
		}
	}

	if existingHook != nil {
		// Check if update is actually needed (unless forceUpdate is set)
		needsUpdate := forceUpdate

		if !needsUpdate {
			// Check if active
			if !existingHook.GetActive() {
				needsUpdate = true
			}

			// Check events - should have workflow_job
			hasWorkflowJob := false
			for _, event := range existingHook.Events {
				if event == "workflow_job" {
					hasWorkflowJob = true
					break
				}
			}
			if !hasWorkflowJob {
				needsUpdate = true
			}

			// Check content_type
			if ct, ok := existingHook.Config["content_type"].(string); !ok || ct != "json" {
				needsUpdate = true
			}
		}

		if !needsUpdate {
			result.Unchanged = true
			return result
		}

		// Update webhook with correct config
		_, _, err := client.Repositories.EditHook(ctx, owner, repo, existingHook.GetID(), &github.Hook{
			Config: map[string]interface{}{
				"content_type": "json",
				"url":          m.webhookURL,
				"secret":       m.webhookSecret,
			},
			Events: []string{"workflow_job"},
			Active: github.Bool(true),
		})
		if err != nil {
			result.Error = fmt.Errorf("failed to update webhook: %w", err)
			m.logger.Error("failed to update webhook", "repo", fullName, "error", err)
			return result
		}

		result.Updated = true
		m.logger.Info("updated webhook", "repo", fullName)
		return result
	}

	// Create new webhook
	hook := &github.Hook{
		Name:   github.String("web"),
		Active: github.Bool(true),
		Events: []string{"workflow_job"},
		Config: map[string]interface{}{
			"content_type": "json",
			"url":          m.webhookURL,
			"secret":       m.webhookSecret,
		},
	}

	_, createResp, err := client.Repositories.CreateHook(ctx, owner, repo, hook)
	if err != nil {
		if createResp != nil {
			switch createResp.StatusCode {
			case 401:
				result.Error = fmt.Errorf("GitHub token is invalid or expired")
				m.logger.Error("GitHub token is invalid or expired", "repo", fullName)
			case 403:
				result.Error = fmt.Errorf("GitHub token lacks webhook write permission (fine-grained PAT needs 'Webhooks: Read and write' on this repo)")
				m.logger.Error("insufficient permissions to create webhook",
					"repo", fullName,
					"hint", "fine-grained PAT needs 'Webhooks: Read and write' permission")
			case 404:
				result.Error = fmt.Errorf("repository not found or no admin access")
				m.logger.Error("repository not found or no admin access", "repo", fullName)
			case 422:
				result.Error = fmt.Errorf("webhook already exists with different configuration")
				m.logger.Error("webhook creation failed - may already exist", "repo", fullName, "error", err)
			default:
				result.Error = fmt.Errorf("failed to create webhook: %w", err)
				m.logger.Error("failed to create webhook", "repo", fullName, "error", err)
			}
		} else {
			result.Error = fmt.Errorf("failed to create webhook: %w", err)
			m.logger.Error("failed to create webhook", "repo", fullName, "error", err)
		}
		return result
	}

	result.Created = true
	m.logger.Info("created webhook", "repo", fullName)
	return result
}

// SyncWebhooks discovers repos and registers webhooks in one operation.
// If forceUpdate is true, all webhooks are updated regardless of current config (used when secret changes).
func (m *Manager) SyncWebhooks(ctx context.Context, discoverer *discovery.Discoverer, forceUpdate bool) ([]RegisterResult, error) {
	repos, err := discoverer.DiscoverRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("discovery failed: %w", err)
	}

	if len(repos) == 0 {
		m.logger.Info("no repositories with self-hosted workflows found")
		return nil, nil
	}

	return m.RegisterWebhooks(ctx, repos, forceUpdate), nil
}
