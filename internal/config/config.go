package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
)

type Config struct {
	// Webhook configuration
	WebhookSecret       string
	WebhookAutoRegister bool
	WebhookURL          string
	WebhookSyncInterval time.Duration

	// GitHub configuration
	GitHubToken   string // Single token for backwards compatibility (GITHUB_TOKEN)
	GitHubTokens  string // JSON array of owner-token pairs (GITHUB_TOKENS)
	RunnerGroupID int64

	// Kubernetes configuration
	Namespace string

	// Runner configuration
	LabelMatchers  string
	RunnerMode     k8s.RunnerMode
	RunnerImage    string
	DindImage      string
	RegistryMirror string
	CachePVC       string
	TTLSeconds     int32
	Tolerations    []corev1.Toleration

	// Resource configuration
	RunnerResources *corev1.ResourceRequirements
	DindResources   *corev1.ResourceRequirements

	// Node configuration
	SkipNodeCheck bool

	// Reconciler configuration
	ReconcilerEnabled          bool
	ReconcilerInterval         time.Duration
	ReconcilerActiveInterval   time.Duration
	ReconcilerInactivityWindow time.Duration

	// Server configuration
	Port string
}

func Load() (*Config, error) {
	cfg := &Config{}

	// Load GitHub tokens - GITHUB_TOKENS takes precedence if set
	cfg.GitHubTokens = os.Getenv("GITHUB_TOKENS")
	cfg.GitHubToken = os.Getenv("GITHUB_TOKEN")

	// At least one token source is required
	if cfg.GitHubTokens == "" && cfg.GitHubToken == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN or GITHUB_TOKENS environment variable is required")
	}

	// Webhook auto-registration
	cfg.WebhookAutoRegister = os.Getenv("WEBHOOK_AUTO_REGISTER") == "true"
	cfg.WebhookURL = os.Getenv("WEBHOOK_URL")

	if cfg.WebhookAutoRegister {
		if cfg.WebhookURL == "" {
			return nil, fmt.Errorf("WEBHOOK_URL is required when WEBHOOK_AUTO_REGISTER is enabled")
		}
		// Webhook secret is optional when auto-registering (will be generated)
		cfg.WebhookSecret = os.Getenv("WEBHOOK_SECRET")

		// Parse sync interval
		syncIntervalStr := os.Getenv("WEBHOOK_SYNC_INTERVAL")
		if syncIntervalStr != "" {
			interval, err := time.ParseDuration(syncIntervalStr)
			if err != nil {
				return nil, fmt.Errorf("invalid WEBHOOK_SYNC_INTERVAL: %s", syncIntervalStr)
			}
			cfg.WebhookSyncInterval = interval
		}
	} else {
		// Manual mode requires webhook secret
		cfg.WebhookSecret = os.Getenv("WEBHOOK_SECRET")
		if cfg.WebhookSecret == "" {
			return nil, fmt.Errorf("WEBHOOK_SECRET environment variable is required (or enable WEBHOOK_AUTO_REGISTER)")
		}
	}

	// Check both RUNNER_NAMESPACE (helm) and NAMESPACE (legacy) for compatibility
	cfg.Namespace = os.Getenv("RUNNER_NAMESPACE")
	if cfg.Namespace == "" {
		cfg.Namespace = os.Getenv("NAMESPACE")
	}
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
		cfg.DindImage = "docker:dind"
	}

	cfg.RegistryMirror = os.Getenv("REGISTRY_MIRROR")

	cfg.CachePVC = os.Getenv("CACHE_PVC")

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

	// Node availability check configuration
	cfg.SkipNodeCheck = os.Getenv("SKIP_NODE_CHECK") == "true"

	// Runner pod tolerations (JSON array format)
	tolerationsStr := os.Getenv("RUNNER_TOLERATIONS")
	if tolerationsStr != "" {
		if err := json.Unmarshal([]byte(tolerationsStr), &cfg.Tolerations); err != nil {
			return nil, fmt.Errorf("invalid RUNNER_TOLERATIONS: %w (must be valid JSON array of toleration objects)", err)
		}
	}

	// Runner container resources (JSON format)
	runnerResourcesStr := os.Getenv("RUNNER_RESOURCES")
	if runnerResourcesStr != "" {
		resources, err := parseResourceRequirements(runnerResourcesStr)
		if err != nil {
			return nil, fmt.Errorf("invalid RUNNER_RESOURCES: %w", err)
		}
		cfg.RunnerResources = resources
	}

	// DinD sidecar resources (JSON format)
	dindResourcesStr := os.Getenv("DIND_RESOURCES")
	if dindResourcesStr != "" {
		resources, err := parseResourceRequirements(dindResourcesStr)
		if err != nil {
			return nil, fmt.Errorf("invalid DIND_RESOURCES: %w", err)
		}
		cfg.DindResources = resources
	}

	// Reconciler configuration - enabled by default
	reconcilerEnabledStr := os.Getenv("RECONCILER_ENABLED")
	cfg.ReconcilerEnabled = reconcilerEnabledStr != "false" // Default to true

	reconcilerIntervalStr := os.Getenv("RECONCILER_INTERVAL")
	if reconcilerIntervalStr != "" {
		interval, err := time.ParseDuration(reconcilerIntervalStr)
		if err != nil {
			return nil, fmt.Errorf("invalid RECONCILER_INTERVAL: %s", reconcilerIntervalStr)
		}
		cfg.ReconcilerInterval = interval
	} else {
		// Default to 5 minutes to avoid rate limiting
		cfg.ReconcilerInterval = 5 * time.Minute
	}

	// Active interval when workflows are queued (faster polling)
	reconcilerActiveIntervalStr := os.Getenv("RECONCILER_ACTIVE_INTERVAL")
	if reconcilerActiveIntervalStr != "" {
		interval, err := time.ParseDuration(reconcilerActiveIntervalStr)
		if err != nil {
			return nil, fmt.Errorf("invalid RECONCILER_ACTIVE_INTERVAL: %s", reconcilerActiveIntervalStr)
		}
		cfg.ReconcilerActiveInterval = interval
	} else {
		cfg.ReconcilerActiveInterval = 60 * time.Second
	}

	// Inactivity window before returning to normal interval
	reconcilerInactivityWindowStr := os.Getenv("RECONCILER_INACTIVITY_WINDOW")
	if reconcilerInactivityWindowStr != "" {
		interval, err := time.ParseDuration(reconcilerInactivityWindowStr)
		if err != nil {
			return nil, fmt.Errorf("invalid RECONCILER_INACTIVITY_WINDOW: %s", reconcilerInactivityWindowStr)
		}
		cfg.ReconcilerInactivityWindow = interval
	} else {
		cfg.ReconcilerInactivityWindow = 10 * time.Minute
	}

	return cfg, nil
}

// parseResourceRequirements parses a JSON string into ResourceRequirements
func parseResourceRequirements(jsonStr string) (*corev1.ResourceRequirements, error) {
	// Define a helper struct for parsing the JSON
	type resourceListJSON struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
	}
	type resourcesJSON struct {
		Requests *resourceListJSON `json:"requests"`
		Limits   *resourceListJSON `json:"limits"`
	}

	var raw resourcesJSON
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	resources := &corev1.ResourceRequirements{
		Requests: make(corev1.ResourceList),
		Limits:   make(corev1.ResourceList),
	}

	// Parse requests
	if raw.Requests != nil {
		if raw.Requests.CPU != "" {
			q, err := resource.ParseQuantity(raw.Requests.CPU)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU request: %w", err)
			}
			resources.Requests[corev1.ResourceCPU] = q
		}
		if raw.Requests.Memory != "" {
			q, err := resource.ParseQuantity(raw.Requests.Memory)
			if err != nil {
				return nil, fmt.Errorf("invalid memory request: %w", err)
			}
			resources.Requests[corev1.ResourceMemory] = q
		}
	}

	// Parse limits
	if raw.Limits != nil {
		if raw.Limits.CPU != "" {
			q, err := resource.ParseQuantity(raw.Limits.CPU)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU limit: %w", err)
			}
			resources.Limits[corev1.ResourceCPU] = q
		}
		if raw.Limits.Memory != "" {
			q, err := resource.ParseQuantity(raw.Limits.Memory)
			if err != nil {
				return nil, fmt.Errorf("invalid memory limit: %w", err)
			}
			resources.Limits[corev1.ResourceMemory] = q
		}
	}

	return resources, nil
}
