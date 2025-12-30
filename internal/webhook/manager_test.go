package webhook

import (
	"fmt"
	"testing"

	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/tokens"
)

func TestGenerateSecret(t *testing.T) {
	// Generate multiple secrets and verify they're unique and properly formatted
	secrets := make(map[string]bool)

	for i := 0; i < 10; i++ {
		secret, err := generateSecret()
		if err != nil {
			t.Fatalf("generateSecret() error = %v", err)
		}

		// Should be 64 hex characters (32 bytes)
		if len(secret) != 64 {
			t.Errorf("generateSecret() length = %d, want 64", len(secret))
		}

		// Should be valid hex
		for _, c := range secret {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("generateSecret() contains invalid hex character: %c", c)
			}
		}

		// Should be unique
		if secrets[secret] {
			t.Errorf("generateSecret() produced duplicate secret")
		}
		secrets[secret] = true
	}
}

func TestNewManager(t *testing.T) {
	log := logger.New()

	// Create a token registry for testing
	tokenRegistry, err := tokens.NewRegistry("", "test-token")
	if err != nil {
		t.Fatalf("failed to create token registry: %v", err)
	}

	tests := []struct {
		name           string
		config         Config
		wantSecretLen  int
		wantSecretSame bool
	}{
		{
			name: "auto-generate secret when empty",
			config: Config{
				TokenRegistry: tokenRegistry,
				WebhookURL:    "https://example.com/webhook",
				WebhookSecret: "",
				Logger:        log,
			},
			wantSecretLen:  64,
			wantSecretSame: false,
		},
		{
			name: "use provided secret",
			config: Config{
				TokenRegistry: tokenRegistry,
				WebhookURL:    "https://example.com/webhook",
				WebhookSecret: "my-custom-secret",
				Logger:        log,
			},
			wantSecretLen:  16, // length of "my-custom-secret"
			wantSecretSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewManager(tt.config)
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}

			secret := m.GetWebhookSecret()

			if len(secret) != tt.wantSecretLen {
				t.Errorf("GetWebhookSecret() length = %d, want %d", len(secret), tt.wantSecretLen)
			}

			if tt.wantSecretSame && secret != tt.config.WebhookSecret {
				t.Errorf("GetWebhookSecret() = %v, want %v", secret, tt.config.WebhookSecret)
			}
		})
	}
}

func TestRegisterResult(t *testing.T) {
	// Test that RegisterResult properly tracks states
	tests := []struct {
		name   string
		result RegisterResult
	}{
		{
			name: "created",
			result: RegisterResult{
				Repo:    "owner/repo",
				Created: true,
			},
		},
		{
			name: "updated",
			result: RegisterResult{
				Repo:    "owner/repo",
				Updated: true,
			},
		},
		{
			name: "unchanged",
			result: RegisterResult{
				Repo:      "owner/repo",
				Unchanged: true,
			},
		},
		{
			name: "error",
			result: RegisterResult{
				Repo:  "owner/repo",
				Error: fmt.Errorf("permission denied"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.result.Repo != "owner/repo" {
				t.Errorf("RegisterResult.Repo = %v, want owner/repo", tt.result.Repo)
			}
		})
	}
}
