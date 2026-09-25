package schemas

import (
	"encoding/json"
	"testing"
)

func TestGithubCopilotOAuthConfigRoundTrip(t *testing.T) {
	original := GithubCopilotKeyConfig{AuthMode: "oauth", AuthClientID: "test-client"}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored GithubCopilotKeyConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.AuthMode != original.AuthMode || restored.AuthClientID != original.AuthClientID {
		t.Fatal("OAuth configuration was lost during JSON round trip")
	}
}

func TestGithubCopilotCanRefresh(t *testing.T) {
	oauth := func(mutate func(*GithubCopilotKeyConfig)) *GithubCopilotKeyConfig {
		config := &GithubCopilotKeyConfig{
			AuthMode:     "oauth",
			AuthClientID: "Iv23li",
			RefreshToken: *NewSecretVar("ghr_token"),
		}
		mutate(config)
		return config
	}

	tests := []struct {
		name   string
		config *GithubCopilotKeyConfig
		want   bool
	}{
		{"complete oauth key", oauth(func(*GithubCopilotKeyConfig) {}), true},
		// GitHub omits the refresh token for apps that opted out of expiring tokens, and
		// for every OAuth App. Those keys are valid, they simply never renew.
		{"no refresh token", oauth(func(c *GithubCopilotKeyConfig) { c.RefreshToken = SecretVar{} }), false},
		// Refreshing posts the client ID alongside the refresh token, so half a pair is
		// unusable rather than merely degraded.
		{"no client id", oauth(func(c *GithubCopilotKeyConfig) { c.AuthClientID = "" }), false},
		{"whitespace client id", oauth(func(c *GithubCopilotKeyConfig) { c.AuthClientID = "   " }), false},
		// App and api_token modes hold no user token to renew.
		{"api token mode", oauth(func(c *GithubCopilotKeyConfig) { c.AuthMode = "api_token" }), false},
		{"github app mode", oauth(func(c *GithubCopilotKeyConfig) { c.AuthMode = "" }), false},
		{"nil config", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.CanRefresh(); got != tt.want {
				t.Fatalf("CanRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGithubCopilotNeedsRefresh(t *testing.T) {
	const now = int64(1_000_000)
	const margin = int64(300)

	tests := []struct {
		name      string
		expiresAt int64
		want      bool
	}{
		{"expired", now - 1, true},
		{"inside the margin", now + margin - 1, true},
		{"exactly at the margin", now + margin, true},
		{"outside the margin", now + margin + 1, false},
		// Zero is GitHub's answer for tokens that never expire, not an unknown expiry.
		// Treating it as "due" would refresh a credential that has no refresh token.
		{"never expires", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &GithubCopilotKeyConfig{AuthMode: "oauth", TokenExpiresAt: tt.expiresAt}
			if got := config.NeedsRefresh(now, margin); got != tt.want {
				t.Fatalf("NeedsRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}
