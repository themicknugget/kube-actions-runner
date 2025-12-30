package discovery

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
)

// Discoverer scans GitHub repositories for self-hosted runner workflows
type Discoverer struct {
	client *github.Client
	logger *logger.Logger
}

// RepoInfo contains information about a repository with self-hosted workflows
type RepoInfo struct {
	Owner     string
	Name      string
	FullName  string
	Labels    []string // Collected labels from runs-on declarations
	Workflows []string // Workflow files containing self-hosted
}

// NewDiscoverer creates a new Discoverer
func NewDiscoverer(token string, log *logger.Logger) *Discoverer {
	return &Discoverer{
		client: github.NewClient(nil).WithAuthToken(token),
		logger: log,
	}
}

// runsOnPattern matches runs-on declarations in workflow files
// Matches both array syntax and string syntax:
//   - runs-on: [self-hosted, linux, x64]
//   - runs-on: self-hosted
var runsOnPattern = regexp.MustCompile(`runs-on:\s*(\[([^\]]+)\]|([^\n]+))`)

// DiscoverRepos finds all repositories with self-hosted runner workflows
func (d *Discoverer) DiscoverRepos(ctx context.Context) ([]RepoInfo, error) {
	d.logger.Info("starting repository discovery")

	user, resp, err := d.client.Users.Get(ctx, "")
	if err != nil {
		if resp != nil && resp.StatusCode == 401 {
			return nil, fmt.Errorf("GitHub token is invalid or expired: %w", err)
		}
		return nil, fmt.Errorf("failed to get authenticated user: %w", err)
	}
	d.logger.Info("authenticated as user", "login", user.GetLogin())

	var allRepos []*github.Repository
	opts := &github.RepositoryListByAuthenticatedUserOptions{
		Affiliation: "owner,collaborator,organization_member",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		repos, resp, err := d.client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			if resp != nil {
				switch resp.StatusCode {
				case 401:
					return nil, fmt.Errorf("GitHub token is invalid or expired: %w", err)
				case 403:
					return nil, fmt.Errorf("GitHub token lacks permission to list repositories (needs 'repo' or 'read:org' scope): %w", err)
				}
			}
			return nil, fmt.Errorf("failed to list repositories: %w", err)
		}
		allRepos = append(allRepos, repos...)

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	d.logger.Info("found repositories", "count", len(allRepos))

	var results []RepoInfo
	for _, repo := range allRepos {
		if repo.GetArchived() {
			continue
		}

		info, err := d.checkRepoForSelfHosted(ctx, repo.GetOwner().GetLogin(), repo.GetName())
		if err != nil {
			d.logger.Warn("failed to check repository",
				"repo", repo.GetFullName(),
				"error", err)
			continue
		}

		if info != nil {
			results = append(results, *info)
			d.logger.Info("found self-hosted workflows",
				"repo", info.FullName,
				"workflows", info.Workflows,
				"labels", info.Labels)
		}
	}

	d.logger.Info("discovery complete", "repos_with_self_hosted", len(results))
	return results, nil
}

// checkRepoForSelfHosted checks if a repository has workflows using self-hosted runners
func (d *Discoverer) checkRepoForSelfHosted(ctx context.Context, owner, repo string) (*RepoInfo, error) {
	// Try to get the .github/workflows directory
	_, dirContent, _, err := d.client.Repositories.GetContents(
		ctx, owner, repo, ".github/workflows", nil)
	if err != nil {
		// No workflows directory - that's fine, just skip
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}

	info := &RepoInfo{
		Owner:    owner,
		Name:     repo,
		FullName: fmt.Sprintf("%s/%s", owner, repo),
	}
	labelSet := make(map[string]struct{})

	for _, file := range dirContent {
		if file.GetType() != "file" {
			continue
		}
		name := file.GetName()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}

		// Get file content
		fileContent, _, _, err := d.client.Repositories.GetContents(
			ctx, owner, repo, file.GetPath(), nil)
		if err != nil {
			d.logger.Warn("failed to get workflow file",
				"repo", info.FullName,
				"file", name,
				"error", err)
			continue
		}

		contentStr, err := fileContent.GetContent()
		if err != nil {
			d.logger.Warn("failed to decode workflow file content",
				"repo", info.FullName,
				"file", name,
				"error", err)
			continue
		}
		content := []byte(contentStr)

		// Check for self-hosted in runs-on
		if labels := d.extractSelfHostedLabels(string(content)); len(labels) > 0 {
			info.Workflows = append(info.Workflows, name)
			for _, l := range labels {
				labelSet[l] = struct{}{}
			}
		}

		// Rate limit protection
		time.Sleep(100 * time.Millisecond)
	}

	if len(info.Workflows) == 0 {
		return nil, nil
	}

	for l := range labelSet {
		info.Labels = append(info.Labels, l)
	}

	return info, nil
}

// extractSelfHostedLabels extracts labels from runs-on declarations containing self-hosted
func (d *Discoverer) extractSelfHostedLabels(content string) []string {
	matches := runsOnPattern.FindAllStringSubmatch(content, -1)
	var allLabels []string

	for _, match := range matches {
		var runsOnValue string
		if match[2] != "" {
			// Array syntax: [self-hosted, linux, x64]
			runsOnValue = match[2]
		} else if match[3] != "" {
			// String syntax: self-hosted
			runsOnValue = match[3]
		} else {
			continue
		}

		// Check if self-hosted is present
		if !strings.Contains(strings.ToLower(runsOnValue), "self-hosted") {
			continue
		}

		// Extract all labels
		labels := strings.Split(runsOnValue, ",")
		for _, l := range labels {
			l = strings.TrimSpace(l)
			l = strings.Trim(l, `"'`)
			if l != "" {
				allLabels = append(allLabels, l)
			}
		}
	}

	return allLabels
}
