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
					AuthMode: mode, OAuthClientID: "public-client-id",
				},
			}}}
			redacted := config.Redacted()
			require.Equal(t, mode, redacted.Keys[0].GithubCopilotKeyConfig.AuthMode)
			require.Equal(t, "public-client-id", redacted.Keys[0].GithubCopilotKeyConfig.OAuthClientID)
			require.NotEqual(t, "test-credential", redacted.Keys[0].Value.GetValue())
			require.Equal(t, "test-credential", config.Keys[0].Value.GetValue())
		})
	}
}
