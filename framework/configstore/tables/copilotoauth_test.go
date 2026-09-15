package tables

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestCopilotOAuthKeyPersistence(t *testing.T) {
	db := setupTestDB(t)
	key := TableKey{Name: "oauth", KeyID: "oauth-id", Provider: "github-copilot", Value: *schemas.NewSecretVar("test-oauth-token"), Models: schemas.WhiteList{"*"}, GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "test-client"}}
	require.NoError(t, db.Create(&key).Error)
	var restored TableKey
	require.NoError(t, db.First(&restored, key.ID).Error)
	require.NotNil(t, restored.GithubCopilotKeyConfig)
	require.Equal(t, "oauth", restored.GithubCopilotKeyConfig.AuthMode)
	require.Equal(t, "test-client", restored.GithubCopilotKeyConfig.OAuthClientID)
	require.Equal(t, "test-oauth-token", restored.Value.GetValue())
	require.Equal(t, "encrypted", restored.EncryptionStatus)
}
