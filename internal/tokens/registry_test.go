package tokens

import (
	"testing"
)

func TestNewRegistry(t *testing.T) {
	tests := []struct {
		name        string
		tokensJSON  string
		defaultTok  string
		wantErr     bool
		errContains string
	}{
		{
			name:       "default token only",
			tokensJSON: "",
			defaultTok: "ghp_default",
			wantErr:    false,
		},
		{
			name:       "tokens JSON only",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"},{"owner":"org1","token":"ghp_org1"}]`,
			defaultTok: "",
			wantErr:    false,
		},
		{
			name:       "both tokens JSON and default",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"}]`,
			defaultTok: "ghp_default",
			wantErr:    false,
		},
		{
			name:        "no tokens configured",
			tokensJSON:  "",
			defaultTok:  "",
			wantErr:     true,
			errContains: "no tokens configured",
		},
		{
			name:        "invalid JSON",
			tokensJSON:  "invalid json",
			defaultTok:  "",
			wantErr:     true,
			errContains: "failed to parse GITHUB_TOKENS",
		},
		{
			name:        "empty owner",
			tokensJSON:  `[{"owner":"","token":"ghp_test"}]`,
			defaultTok:  "",
			wantErr:     true,
			errContains: "empty owner",
		},
		{
			name:        "empty token",
			tokensJSON:  `[{"owner":"user1","token":""}]`,
			defaultTok:  "",
			wantErr:     true,
			errContains: "empty token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(tt.tokensJSON, tt.defaultTok)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
				} else if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if registry == nil {
				t.Error("expected non-nil registry")
			}
		})
	}
}

func TestGetTokenForOwner(t *testing.T) {
	tests := []struct {
		name       string
		tokensJSON string
		defaultTok string
		owner      string
		wantToken  string
		wantErr    bool
	}{
		{
			name:       "get owner-specific token",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"},{"owner":"org1","token":"ghp_org1"}]`,
			defaultTok: "ghp_default",
			owner:      "user1",
			wantToken:  "ghp_user1",
			wantErr:    false,
		},
		{
			name:       "case insensitive owner lookup",
			tokensJSON: `[{"owner":"User1","token":"ghp_user1"}]`,
			defaultTok: "",
			owner:      "user1",
			wantToken:  "ghp_user1",
			wantErr:    false,
		},
		{
			name:       "fallback to default token",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"}]`,
			defaultTok: "ghp_default",
			owner:      "unknown-owner",
			wantToken:  "ghp_default",
			wantErr:    false,
		},
		{
			name:       "default token only",
			tokensJSON: "",
			defaultTok: "ghp_default",
			owner:      "any-owner",
			wantToken:  "ghp_default",
			wantErr:    false,
		},
		{
			name:       "no token for owner without default",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"}]`,
			defaultTok: "",
			owner:      "unknown-owner",
			wantToken:  "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(tt.tokensJSON, tt.defaultTok)
			if err != nil {
				t.Fatalf("failed to create registry: %v", err)
			}

			token, err := registry.GetTokenForOwner(tt.owner)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if token != tt.wantToken {
				t.Errorf("expected token %q, got %q", tt.wantToken, token)
			}
		})
	}
}

func TestGetAllTokens(t *testing.T) {
	tests := []struct {
		name       string
		tokensJSON string
		defaultTok string
		wantCount  int
	}{
		{
			name:       "multiple owner tokens",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"},{"owner":"org1","token":"ghp_org1"}]`,
			defaultTok: "",
			wantCount:  2,
		},
		{
			name:       "default token only",
			tokensJSON: "",
			defaultTok: "ghp_default",
			wantCount:  1,
		},
		{
			name:       "single owner token",
			tokensJSON: `[{"owner":"user1","token":"ghp_user1"}]`,
			defaultTok: "ghp_default",
			wantCount:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(tt.tokensJSON, tt.defaultTok)
			if err != nil {
				t.Fatalf("failed to create registry: %v", err)
			}

			tokens := registry.GetAllTokens()
			if len(tokens) != tt.wantCount {
				t.Errorf("expected %d tokens, got %d", tt.wantCount, len(tokens))
			}
		})
	}
}

func TestGetConfiguredOwners(t *testing.T) {
	registry, err := NewRegistry(`[{"owner":"user1","token":"ghp_user1"},{"owner":"org1","token":"ghp_org1"}]`, "")
	if err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	owners := registry.GetConfiguredOwners()
	if len(owners) != 2 {
		t.Errorf("expected 2 owners, got %d", len(owners))
	}

	// Check that owners are present
	ownerMap := make(map[string]bool)
	for _, o := range owners {
		ownerMap[o] = true
	}
	if !ownerMap["user1"] {
		t.Error("expected user1 in owners")
	}
	if !ownerMap["org1"] {
		t.Error("expected org1 in owners")
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
