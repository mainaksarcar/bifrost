package tables

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCopilotOAuthKeyPersistence(t *testing.T) {
	db := setupTestDB(t)
	key := TableKey{Name: "oauth", KeyID: "oauth-id", Provider: "github-copilot", Value: *schemas.NewSecretVar("test-oauth-token"), Models: schemas.WhiteList{"*"}, GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", AuthClientID: "test-client"}}
	require.NoError(t, db.Create(&key).Error)
	var restored TableKey
	require.NoError(t, db.First(&restored, key.ID).Error)
	require.NotNil(t, restored.GithubCopilotKeyConfig)
	require.Equal(t, "oauth", restored.GithubCopilotKeyConfig.AuthMode)
	require.Equal(t, "test-client", restored.GithubCopilotKeyConfig.AuthClientID)
	require.Equal(t, "test-oauth-token", restored.Value.GetValue())
	require.Equal(t, "encrypted", restored.EncryptionStatus)
}

// Refreshing rotates both tokens, so a refresh token that failed to round-trip would
// leave the key unable to renew and force a fresh device login eight hours later.
func TestCopilotOAuthRefreshTokenPersistence(t *testing.T) {
	db := setupTestDB(t)
	const expiresAt = int64(1789543415)
	key := TableKey{
		Name: "oauth-refresh", KeyID: "oauth-refresh-id", Provider: "github-copilot",
		Value: *schemas.NewSecretVar("test-oauth-token"), Models: schemas.WhiteList{"*"},
		GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AuthMode: "oauth", AuthClientID: "test-client",
			RefreshToken: *schemas.NewSecretVar("ghr_testrefreshtoken"), TokenExpiresAt: expiresAt,
		},
	}
	require.NoError(t, db.Create(&key).Error)
	var restored TableKey
	require.NoError(t, db.First(&restored, key.ID).Error)
	require.NotNil(t, restored.GithubCopilotKeyConfig)
	require.Equal(t, "ghr_testrefreshtoken", restored.GithubCopilotKeyConfig.RefreshToken.GetValue())
	require.Equal(t, expiresAt, restored.GithubCopilotKeyConfig.TokenExpiresAt)
	require.Equal(t, "encrypted", restored.EncryptionStatus)
}
