package tokens

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OwnerToken represents an owner-token pair
type OwnerToken struct {
	Owner string `json:"owner"`
	Token string `json:"token"`
}

// Registry manages tokens for different GitHub owners
type Registry struct {
	tokensByOwner map[string]string
	defaultToken  string
	ownerTokens   []OwnerToken
}

// NewRegistry creates a new token registry from configuration
// If tokensJSON is provided (GITHUB_TOKENS), it takes precedence
// Otherwise, defaultToken (GITHUB_TOKEN) is used as fallback for all owners
func NewRegistry(tokensJSON string, defaultToken string) (*Registry, error) {
	r := &Registry{
		tokensByOwner: make(map[string]string),
		defaultToken:  defaultToken,
	}

	// Parse GITHUB_TOKENS if provided
	if tokensJSON != "" {
		var tokens []OwnerToken
		if err := json.Unmarshal([]byte(tokensJSON), &tokens); err != nil {
			return nil, fmt.Errorf("failed to parse GITHUB_TOKENS: %w", err)
		}

		for _, t := range tokens {
			if t.Owner == "" {
				return nil, fmt.Errorf("GITHUB_TOKENS contains entry with empty owner")
			}
			if t.Token == "" {
				return nil, fmt.Errorf("GITHUB_TOKENS contains entry with empty token for owner %s", t.Owner)
			}
			// Store with lowercase owner for case-insensitive matching
			r.tokensByOwner[strings.ToLower(t.Owner)] = t.Token
			r.ownerTokens = append(r.ownerTokens, t)
		}
	}

	// Validate we have at least one token source
	if len(r.tokensByOwner) == 0 && r.defaultToken == "" {
		return nil, fmt.Errorf("no tokens configured: set GITHUB_TOKENS or GITHUB_TOKEN")
	}

	return r, nil
}

// GetTokenForOwner returns the token for a specific owner
// If owner not found but a default token exists, returns that
// If no token found at all, returns an error
func (r *Registry) GetTokenForOwner(owner string) (string, error) {
	// Try owner-specific token first (case-insensitive)
	if token, ok := r.tokensByOwner[strings.ToLower(owner)]; ok {
		return token, nil
	}

	// Fall back to default token
	if r.defaultToken != "" {
		return r.defaultToken, nil
	}

	return "", fmt.Errorf("no token configured for owner %s", owner)
}

// GetAllTokens returns all configured owner-token pairs
// If only defaultToken is configured, returns a single entry with empty owner
func (r *Registry) GetAllTokens() []OwnerToken {
	if len(r.ownerTokens) > 0 {
		return r.ownerTokens
	}

	// Return default token if no owner-specific tokens
	if r.defaultToken != "" {
		return []OwnerToken{{Owner: "", Token: r.defaultToken}}
	}

	return nil
}

// HasMultipleTokens returns true if multiple owner-specific tokens are configured
func (r *Registry) HasMultipleTokens() bool {
	return len(r.ownerTokens) > 1
}

// GetConfiguredOwners returns a list of configured owner names
func (r *Registry) GetConfiguredOwners() []string {
	owners := make([]string, 0, len(r.ownerTokens))
	for _, ot := range r.ownerTokens {
		owners = append(owners, ot.Owner)
	}
	return owners
}

// HasDefaultToken returns true if a default token is configured
func (r *Registry) HasDefaultToken() bool {
	return r.defaultToken != ""
}
