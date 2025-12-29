package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
)

type Config struct {
	WebhookSecret string
	GitHubToken   string
	Namespace     string
	LabelMatchers string
	Port          string
	RunnerMode    k8s.RunnerMode
	RunnerImage   string
	DindImage     string
	TTLSeconds    int32
	RunnerGroupID int64
}

func Load() (*Config, error) {
	cfg := &Config{}

	cfg.WebhookSecret = os.Getenv("WEBHOOK_SECRET")
	if cfg.WebhookSecret == "" {
		return nil, fmt.Errorf("WEBHOOK_SECRET environment variable is required")
	}

	cfg.GitHubToken = os.Getenv("GITHUB_TOKEN")
	if cfg.GitHubToken == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN environment variable is required")
	}

	cfg.Namespace = os.Getenv("NAMESPACE")
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}

	cfg.LabelMatchers = os.Getenv("LABEL_MATCHERS")

	cfg.Port = os.Getenv("PORT")
	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	runnerModeStr := os.Getenv("RUNNER_MODE")
	if runnerModeStr == "" {
		cfg.RunnerMode = k8s.RunnerModeDinDRootless
	} else {
		if !k8s.IsValidRunnerMode(runnerModeStr) {
			return nil, fmt.Errorf("invalid RUNNER_MODE: %s (must be one of: %v)", runnerModeStr, k8s.ValidRunnerModes())
		}
		cfg.RunnerMode = k8s.RunnerMode(runnerModeStr)
	}

	cfg.RunnerImage = os.Getenv("RUNNER_IMAGE")
	if cfg.RunnerImage == "" {
		cfg.RunnerImage = k8s.DefaultImageForMode(string(cfg.RunnerMode))
	}

	cfg.DindImage = os.Getenv("DIND_IMAGE")
	if cfg.DindImage == "" {
		cfg.DindImage = k8s.DefaultDinDSidecarImage
	}

	ttlSecondsStr := os.Getenv("JOB_TTL_SECONDS")
	if ttlSecondsStr != "" {
		ttl, err := strconv.ParseInt(ttlSecondsStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid JOB_TTL_SECONDS: %s (must be a valid integer)", ttlSecondsStr)
		}
		cfg.TTLSeconds = int32(ttl)
	} else {
		cfg.TTLSeconds = 300
	}

	runnerGroupIDStr := os.Getenv("RUNNER_GROUP_ID")
	if runnerGroupIDStr != "" {
		groupID, err := strconv.ParseInt(runnerGroupIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid RUNNER_GROUP_ID: %s (must be a valid integer)", runnerGroupIDStr)
		}
		cfg.RunnerGroupID = groupID
	} else {
		cfg.RunnerGroupID = 1
	}

	return cfg, nil
}
