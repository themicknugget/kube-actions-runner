package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	// Save original env and restore after test
	envVars := []string{
		"GITHUB_TOKEN",
		"WEBHOOK_SECRET",
		"WEBHOOK_AUTO_REGISTER",
		"WEBHOOK_URL",
		"WEBHOOK_SYNC_INTERVAL",
		"RUNNER_NAMESPACE",
		"NAMESPACE",
		"RUNNER_MODE",
		"JOB_TTL_SECONDS",
	}
	originalEnv := make(map[string]string)
	for _, k := range envVars {
		originalEnv[k] = os.Getenv(k)
	}
	defer func() {
		for k, v := range originalEnv {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}()

	clearEnv := func() {
		for _, k := range envVars {
			os.Unsetenv(k)
		}
	}

	tests := []struct {
		name        string
		env         map[string]string
		wantErr     bool
		errContains string
		validate    func(*Config) error
	}{
		{
			name:        "missing github token",
			env:         map[string]string{},
			wantErr:     true,
			errContains: "GITHUB_TOKEN",
		},
		{
			name: "missing webhook secret without auto-register",
			env: map[string]string{
				"GITHUB_TOKEN": "test-token",
			},
			wantErr:     true,
			errContains: "WEBHOOK_SECRET",
		},
		{
			name: "manual mode with webhook secret",
			env: map[string]string{
				"GITHUB_TOKEN":   "test-token",
				"WEBHOOK_SECRET": "test-secret",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.WebhookAutoRegister {
					return fmt.Errorf("expected WebhookAutoRegister=false")
				}
				if c.WebhookSecret != "test-secret" {
					return fmt.Errorf("expected WebhookSecret=test-secret, got %s", c.WebhookSecret)
				}
				return nil
			},
		},
		{
			name: "auto-register without webhook URL",
			env: map[string]string{
				"GITHUB_TOKEN":          "test-token",
				"WEBHOOK_AUTO_REGISTER": "true",
			},
			wantErr:     true,
			errContains: "WEBHOOK_URL",
		},
		{
			name: "auto-register with webhook URL",
			env: map[string]string{
				"GITHUB_TOKEN":          "test-token",
				"WEBHOOK_AUTO_REGISTER": "true",
				"WEBHOOK_URL":           "https://example.com/webhook",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if !c.WebhookAutoRegister {
					return fmt.Errorf("expected WebhookAutoRegister=true")
				}
				if c.WebhookURL != "https://example.com/webhook" {
					return fmt.Errorf("expected WebhookURL=https://example.com/webhook, got %s", c.WebhookURL)
				}
				// WebhookSecret should be empty (will be auto-generated at runtime)
				if c.WebhookSecret != "" {
					return fmt.Errorf("expected WebhookSecret to be empty, got %s", c.WebhookSecret)
				}
				return nil
			},
		},
		{
			name: "auto-register with optional webhook secret",
			env: map[string]string{
				"GITHUB_TOKEN":          "test-token",
				"WEBHOOK_AUTO_REGISTER": "true",
				"WEBHOOK_URL":           "https://example.com/webhook",
				"WEBHOOK_SECRET":        "custom-secret",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.WebhookSecret != "custom-secret" {
					return fmt.Errorf("expected WebhookSecret=custom-secret, got %s", c.WebhookSecret)
				}
				return nil
			},
		},
		{
			name: "auto-register with sync interval",
			env: map[string]string{
				"GITHUB_TOKEN":          "test-token",
				"WEBHOOK_AUTO_REGISTER": "true",
				"WEBHOOK_URL":           "https://example.com/webhook",
				"WEBHOOK_SYNC_INTERVAL": "1h",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.WebhookSyncInterval != time.Hour {
					return fmt.Errorf("expected WebhookSyncInterval=1h, got %v", c.WebhookSyncInterval)
				}
				return nil
			},
		},
		{
			name: "invalid sync interval",
			env: map[string]string{
				"GITHUB_TOKEN":          "test-token",
				"WEBHOOK_AUTO_REGISTER": "true",
				"WEBHOOK_URL":           "https://example.com/webhook",
				"WEBHOOK_SYNC_INTERVAL": "invalid",
			},
			wantErr:     true,
			errContains: "WEBHOOK_SYNC_INTERVAL",
		},
		{
			name: "RUNNER_NAMESPACE takes precedence over NAMESPACE",
			env: map[string]string{
				"GITHUB_TOKEN":     "test-token",
				"WEBHOOK_SECRET":   "test-secret",
				"RUNNER_NAMESPACE": "runners-ns",
				"NAMESPACE":        "old-ns",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.Namespace != "runners-ns" {
					return fmt.Errorf("expected Namespace=runners-ns, got %s", c.Namespace)
				}
				return nil
			},
		},
		{
			name: "NAMESPACE fallback when RUNNER_NAMESPACE not set",
			env: map[string]string{
				"GITHUB_TOKEN":   "test-token",
				"WEBHOOK_SECRET": "test-secret",
				"NAMESPACE":      "legacy-ns",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.Namespace != "legacy-ns" {
					return fmt.Errorf("expected Namespace=legacy-ns, got %s", c.Namespace)
				}
				return nil
			},
		},
		{
			name: "default namespace when none set",
			env: map[string]string{
				"GITHUB_TOKEN":   "test-token",
				"WEBHOOK_SECRET": "test-secret",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.Namespace != "default" {
					return fmt.Errorf("expected Namespace=default, got %s", c.Namespace)
				}
				return nil
			},
		},
		{
			name: "default TTL seconds",
			env: map[string]string{
				"GITHUB_TOKEN":   "test-token",
				"WEBHOOK_SECRET": "test-secret",
			},
			wantErr: false,
			validate: func(c *Config) error {
				if c.TTLSeconds != 300 {
					return fmt.Errorf("expected TTLSeconds=300, got %d", c.TTLSeconds)
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv()
			for k, v := range tt.env {
				os.Setenv(k, v)
			}

			cfg, err := Load()

			if tt.wantErr {
				if err == nil {
					t.Errorf("Load() expected error containing %q, got nil", tt.errContains)
					return
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Load() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("Load() unexpected error = %v", err)
				return
			}

			if tt.validate != nil {
				if err := tt.validate(cfg); err != nil {
					t.Errorf("Load() validation failed: %v", err)
				}
			}
		})
	}
}
