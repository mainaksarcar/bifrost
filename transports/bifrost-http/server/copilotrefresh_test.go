package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// copilotRecordingStore embeds the ConfigStore interface so only the methods the refresh
// pass touches need real bodies; any other call panics, which is what proves a gated or
// skipped pass never reached the store.
type copilotRecordingStore struct {
	configstore.ConfigStore
	keys     []schemas.Key
	reads    int
	updated  []schemas.Key
	statuses []string
	updateNo error
}

func (s *copilotRecordingStore) GetProviderKeys(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	s.reads++
	return s.keys, nil
}

func (s *copilotRecordingStore) UpdateProviderKey(_ context.Context, _ schemas.ModelProvider, _ string, key schemas.Key, _ ...*gorm.DB) error {
	if s.updateNo != nil {
		return s.updateNo
	}
	s.updated = append(s.updated, key)
	return nil
}

func (s *copilotRecordingStore) UpdateStatus(_ context.Context, _ schemas.ModelProvider, _ string, status, description string) error {
	s.statuses = append(s.statuses, status+": "+description)
	return nil
}

func oauthKey(id string, expiresAt int64) schemas.Key {
	return schemas.Key{
		ID:    id,
		Value: *schemas.NewSecretVar("ghu_old_access"),
		GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AuthMode:       "oauth",
			AuthClientID:   "Iv23li",
			RefreshToken:   *schemas.NewSecretVar("ghr_old_refresh"),
			TokenExpiresAt: expiresAt,
		},
	}
}

func newTestRefreshWorker(t *testing.T, store configstore.ConfigStore, body any, gate func() bool) *copilotTokenRefreshWorker {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	worker := newCopilotTokenRefreshWorker(store, bifrost.NewNoOpLogger(), gate)
	require.NotNil(t, worker)
	worker.tokenURL = server.URL
	return worker
}

func TestCopilotRefreshWorkerGate(t *testing.T) {
	tests := []struct {
		name      string
		gate      func() bool
		wantReads int
	}{
		{"nil gate always refreshes", nil, 1},
		{"true gate refreshes", func() bool { return true }, 1},
		{"false gate skips before touching the store", func() bool { return false }, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &copilotRecordingStore{}
			worker := newTestRefreshWorker(t, store, map[string]any{}, tt.gate)
			worker.refreshDueKeys(context.Background())
			require.Equal(t, tt.wantReads, store.reads)
		})
	}
}

func TestCopilotRefreshWorkerSelectsOnlyDueRenewableKeys(t *testing.T) {
	now := time.Now().Unix()
	expired := oauthKey("expired", now-60)

	// OAuth Apps and apps that opted out of expiring tokens report no expiry and no
	// refresh token. They are healthy keys, not candidates.
	neverExpires := oauthKey("never-expires", 0)
	noRefreshToken := oauthKey("no-refresh-token", now-60)
	noRefreshToken.GithubCopilotKeyConfig.RefreshToken = schemas.SecretVar{}

	// The GitHub App and pre-minted-token modes hold no user token to renew.
	appMode := oauthKey("app-mode", now-60)
	appMode.GithubCopilotKeyConfig.AuthMode = ""
	apiTokenMode := oauthKey("api-token-mode", now-60)
	apiTokenMode.GithubCopilotKeyConfig.AuthMode = "api_token"

	notDueYet := oauthKey("not-due-yet", now+int64((2*time.Hour)/time.Second))

	store := &copilotRecordingStore{keys: []schemas.Key{
		expired, neverExpires, noRefreshToken, appMode, apiTokenMode, notDueYet,
	}}
	worker := newTestRefreshWorker(t, store, map[string]any{
		"access_token": "ghu_new_access", "refresh_token": "ghr_new_refresh", "expires_in": 28800,
	}, nil)

	worker.refreshDueKeys(context.Background())

	require.Len(t, store.updated, 1, "only the expired renewable key should be renewed")
	require.Equal(t, "expired", store.updated[0].ID)
}

func TestCopilotRefreshWorkerPersistsRotatedPair(t *testing.T) {
	store := &copilotRecordingStore{keys: []schemas.Key{oauthKey("k1", time.Now().Unix()-60)}}
	worker := newTestRefreshWorker(t, store, map[string]any{
		"access_token": "ghu_new_access", "refresh_token": "ghr_new_refresh", "expires_in": 28800,
	}, nil)

	before := time.Now().Unix()
	worker.refreshDueKeys(context.Background())
	require.Len(t, store.updated, 1)

	renewed := store.updated[0]
	require.Equal(t, "ghu_new_access", renewed.Value.GetValue())
	// GitHub invalidates the old refresh token the moment this one is issued, so keeping
	// the old value would break every future renewal rather than just this one.
	require.Equal(t, "ghr_new_refresh", renewed.GithubCopilotKeyConfig.RefreshToken.GetValue())
	require.GreaterOrEqual(t, renewed.GithubCopilotKeyConfig.TokenExpiresAt, before+28800)
}

func TestCopilotRefreshWorkerRejectedRefreshMarksKeyForReauthorization(t *testing.T) {
	// incorrect_client_credentials is what GitHub actually answers an OAuth App with,
	// since only a GitHub App device-flow grant is exempt from sending a client secret.
	// Treating it as transient left the key retrying every interval and never surfacing.
	for _, reason := range []string{"bad_refresh_token", "invalid_grant", "incorrect_client_credentials", "unauthorized_client"} {
		t.Run(reason, func(t *testing.T) {
			store := &copilotRecordingStore{keys: []schemas.Key{oauthKey("k1", time.Now().Unix()-60)}}
			worker := newTestRefreshWorker(t, store, map[string]any{"error": reason}, nil)

			worker.refreshDueKeys(context.Background())

			require.Empty(t, store.updated, "a rejected refresh must not overwrite the stored credential")
			require.Len(t, store.statuses, 1)
			require.Contains(t, store.statuses[0], "re-authorise")
		})
	}
}

// An OAuth App can never refresh here, so naming the app type is the difference
// between an operator re-authenticating every eight hours forever and switching to
// a GitHub App. The raw GitHub error code alone does not carry that.
func TestCopilotRefreshWorkerExplainsOAuthAppCredentialRejection(t *testing.T) {
	oauthAppKey := func() schemas.Key {
		key := oauthKey("k1", time.Now().Unix()-60)
		key.GithubCopilotKeyConfig.AuthClientID = "Ov23lizRcFDByb2bOwYh"
		return key
	}

	t.Run("oauth app credential rejection names the cause", func(t *testing.T) {
		store := &copilotRecordingStore{keys: []schemas.Key{oauthAppKey()}}
		worker := newTestRefreshWorker(t, store, map[string]any{"error": "incorrect_client_credentials"}, nil)

		worker.refreshDueKeys(context.Background())

		require.Len(t, store.statuses, 1)
		require.Contains(t, store.statuses[0], "OAuth App")
		require.Contains(t, store.statuses[0], "client secret")
		require.Contains(t, store.statuses[0], "GitHub App")
	})

	t.Run("github app keeps the plain message", func(t *testing.T) {
		store := &copilotRecordingStore{keys: []schemas.Key{oauthKey("k1", time.Now().Unix()-60)}}
		worker := newTestRefreshWorker(t, store, map[string]any{"error": "incorrect_client_credentials"}, nil)

		worker.refreshDueKeys(context.Background())

		require.Len(t, store.statuses, 1)
		require.NotContains(t, store.statuses[0], "OAuth App",
			"a GitHub App is exempt from the client secret, so this advice would misdirect")
	})

	t.Run("spent refresh chain is not blamed on the app type", func(t *testing.T) {
		store := &copilotRecordingStore{keys: []schemas.Key{oauthAppKey()}}
		worker := newTestRefreshWorker(t, store, map[string]any{"error": "bad_refresh_token"}, nil)

		worker.refreshDueKeys(context.Background())

		require.Len(t, store.statuses, 1)
		require.NotContains(t, store.statuses[0], "client secret",
			"an expired refresh chain would still fail with a secret, so naming it misleads")
	})
}

func TestCopilotRefreshWorkerReportsLostCredentialOnWriteFailure(t *testing.T) {
	store := &copilotRecordingStore{
		keys:     []schemas.Key{oauthKey("k1", time.Now().Unix()-60)},
		updateNo: fmt.Errorf("database unavailable"),
	}
	worker := newTestRefreshWorker(t, store, map[string]any{
		"access_token": "ghu_new_access", "refresh_token": "ghr_new_refresh", "expires_in": 28800,
	}, nil)

	// GitHub has already retired the stored pair by this point, so a failed write loses
	// the credential outright. The pass must surface that rather than swallow it.
	err := worker.refreshKey(context.Background(), store.keys[0])
	require.Error(t, err)
	require.Empty(t, store.updated)
}
