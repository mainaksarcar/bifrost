package configstore

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCopilotOAuthRedactionPreservesAuthenticationMetadata(t *testing.T) {
	for _, mode := range []string{"oauth", "api_token"} {
		t.Run(mode, func(t *testing.T) {
			config := ProviderConfig{Keys: []schemas.Key{{
				Value: *schemas.NewSecretVar("test-credential"),
				GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
					AuthMode: mode, AuthClientID: "public-client-id",
				},
			}}}
			redacted := config.Redacted()
			require.Equal(t, mode, redacted.Keys[0].GithubCopilotKeyConfig.AuthMode)
			require.Equal(t, "public-client-id", redacted.Keys[0].GithubCopilotKeyConfig.AuthClientID)
			require.NotEqual(t, "test-credential", redacted.Keys[0].Value.GetValue())
			require.Equal(t, "test-credential", config.Keys[0].Value.GetValue())
		})
	}
}

// A refresh token is a six-month credential, so a config read must not hand it back
// even though the expiry beside it is ordinary metadata the UI needs.
func TestCopilotOAuthRedactionHidesRefreshToken(t *testing.T) {
	const expiresAt = int64(1789543415)
	config := ProviderConfig{Keys: []schemas.Key{{
		Value: *schemas.NewSecretVar("test-credential"),
		GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AuthMode: "oauth", AuthClientID: "public-client-id",
			RefreshToken: *schemas.NewSecretVar("ghr_testrefreshtoken"), TokenExpiresAt: expiresAt,
		},
	}}}
	redacted := config.Redacted()
	require.NotEqual(t, "ghr_testrefreshtoken", redacted.Keys[0].GithubCopilotKeyConfig.RefreshToken.GetValue())
	require.Equal(t, expiresAt, redacted.Keys[0].GithubCopilotKeyConfig.TokenExpiresAt)
	require.Equal(t, "ghr_testrefreshtoken", config.Keys[0].GithubCopilotKeyConfig.RefreshToken.GetValue())
}
