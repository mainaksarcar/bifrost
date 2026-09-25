package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

const (
	copilotRefreshInterval = 5 * time.Minute
	// GitHub issues eight-hour access tokens. Renewing well ahead of expiry keeps a
	// slow tick, a restart, or a brief GitHub outage from stranding a live key.
	copilotRefreshMargin   = 15 * time.Minute
	copilotRefreshTokenURL = "https://github.com/login/oauth/access_token"
	copilotRefreshMaxBody  = 65536
)

// copilotTokenRefreshWorker renews GitHub Copilot device-login credentials before they
// expire, so an operator does not have to re-authorise in a browser every eight hours.
//
// It lives here rather than in the provider because refreshing rotates the credential:
// GitHub invalidates the old access token and the old refresh token the moment a refresh
// succeeds. The new pair therefore has to be persisted, and the provider has no config
// store to persist it to.
type copilotTokenRefreshWorker struct {
	store    configstore.ConfigStore
	logger   schemas.Logger
	client   *http.Client
	tokenURL string
	interval time.Duration
	margin   time.Duration
	// shouldRefresh is consulted before each pass; when it returns false the pass is
	// skipped. Rotation makes concurrent refreshes of one key actively harmful — the
	// slower writer would persist a refresh token GitHub has already invalidated — so
	// deployments sharing a config store elect a single refresher. nil means always run.
	shouldRefresh func() bool
	stopCh        chan struct{}
	stopOnce      sync.Once
	cancel        context.CancelFunc
}

func newCopilotTokenRefreshWorker(store configstore.ConfigStore, logger schemas.Logger, shouldRefresh func() bool) *copilotTokenRefreshWorker {
	if store == nil || logger == nil {
		return nil
	}
	return &copilotTokenRefreshWorker{
		store:         store,
		logger:        logger,
		client:        &http.Client{Timeout: 30 * time.Second},
		tokenURL:      copilotRefreshTokenURL,
		interval:      copilotRefreshInterval,
		margin:        copilotRefreshMargin,
		shouldRefresh: shouldRefresh,
		stopCh:        make(chan struct{}),
	}
}

func (w *copilotTokenRefreshWorker) start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.run(runCtx)
}

func (w *copilotTokenRefreshWorker) stop() {
	w.stopOnce.Do(func() {
		// Cancel any in-flight refresh so a blocked DB or HTTP call unwinds promptly,
		// then signal run() to exit its ticker loop.
		if w.cancel != nil {
			w.cancel()
		}
		close(w.stopCh)
	})
}

func (w *copilotTokenRefreshWorker) run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	// A gateway that was down longer than the token's life comes back with an expired
	// credential, so sweep immediately rather than waiting out the first interval.
	w.refreshDueKeys(ctx)
	for {
		select {
		case <-ticker.C:
			w.refreshDueKeys(ctx)
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (w *copilotTokenRefreshWorker) refreshDueKeys(ctx context.Context) {
	if w.shouldRefresh != nil && !w.shouldRefresh() {
		return
	}
	keys, err := w.store.GetProviderKeys(ctx, schemas.GithubCopilot)
	if err != nil {
		w.logger.Warn("copilot token refresh: cannot read keys: %v", err)
		return
	}
	now := time.Now().Unix()
	margin := int64(w.margin / time.Second)
	for _, key := range keys {
		config := key.GithubCopilotKeyConfig
		// Keys without a refresh token are not broken: OAuth Apps, and GitHub Apps that
		// opted out of expiring tokens, hold credentials that simply never expire.
		if !config.CanRefresh() || !config.NeedsRefresh(now, margin) {
			continue
		}
		if err := w.refreshKey(ctx, key); err != nil {
			w.logger.Warn("copilot token refresh: key %s not renewed: %v", key.ID, err)
		}
	}
}

type copilotRefreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func (w *copilotTokenRefreshWorker) refreshKey(ctx context.Context, key schemas.Key) error {
	config := key.GithubCopilotKeyConfig
	result, err := w.exchange(ctx, config.AuthClientID, config.RefreshToken.GetValue())
	if err != nil {
		return err
	}
	if result.Error != "" {
		// None of these clear on a retry: the refresh chain is spent, or the app cannot
		// authenticate the grant at all. Retrying every interval would keep the key
		// looking merely unlucky while it is in fact unusable until someone acts.
		switch result.Error {
		case "bad_refresh_token", "invalid_grant", "incorrect_client_credentials", "unauthorized_client":
			w.markNeedsReauthorization(ctx, key, result.Error)
		}
		return fmt.Errorf("github rejected the refresh: %s", result.Error)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return fmt.Errorf("github returned no access token")
	}

	renewed := key
	renewedConfig := *config
	renewed.Value = *schemas.NewSecretVar(result.AccessToken)
	// GitHub rotates the refresh token on every use. Persisting the old one would leave
	// the key holding a credential GitHub has already invalidated.
	if result.RefreshToken != "" {
		renewedConfig.RefreshToken = *schemas.NewSecretVar(result.RefreshToken)
	}
	if result.ExpiresIn > 0 {
		renewedConfig.TokenExpiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).Unix()
	}
	renewed.GithubCopilotKeyConfig = &renewedConfig

	if err := w.store.UpdateProviderKey(ctx, schemas.GithubCopilot, key.ID, renewed); err != nil {
		// The rotated pair exists only in this function's locals, and GitHub has already
		// retired the pair still in the database. Losing this write costs the credential,
		// so it is an error rather than a retryable warning.
		w.logger.Error("copilot token refresh: key %s renewed at GitHub but the new "+
			"credential could not be stored; the key now needs re-authorisation: %v", key.ID, err)
		return err
	}
	w.logger.Info("copilot token refresh: renewed key %s", key.ID)
	return nil
}

// markNeedsReauthorization records why a key stopped working, so the cause is visible in
// the UI instead of surfacing later as an unexplained inference failure.
func (w *copilotTokenRefreshWorker) markNeedsReauthorization(ctx context.Context, key schemas.Key, reason string) {
	message := fmt.Sprintf("GitHub rejected the refresh token (%s); re-authorise this key with a new device login", reason)
	if isCredentialRejection(reason) && isOAuthAppClientID(key.GithubCopilotKeyConfig.AuthClientID) {
		message = fmt.Sprintf("GitHub rejected the refresh (%s) because client ID %s belongs to an OAuth App, "+
			"whose refresh grant requires a client secret that Bifrost does not store. Re-authorise this key with a "+
			"new device login, or register a GitHub App instead, whose device-flow tokens refresh without a secret.",
			reason, key.GithubCopilotKeyConfig.AuthClientID)
	}
	if err := w.store.UpdateStatus(ctx, schemas.GithubCopilot, key.ID, "error", message); err != nil {
		w.logger.Warn("copilot token refresh: cannot record re-authorisation status for key %s: %v", key.ID, err)
	}
}

// isCredentialRejection reports whether GitHub refused the grant itself rather than the
// refresh token, which is the failure an OAuth App's missing client secret produces.
func isCredentialRejection(reason string) bool {
	return reason == "incorrect_client_credentials" || reason == "unauthorized_client"
}

// isOAuthAppClientID distinguishes the two app types by GitHub's client ID prefixes:
// GitHub Apps are issued Iv..., OAuth Apps Ov....
func isOAuthAppClientID(clientID string) bool {
	return strings.HasPrefix(clientID, "Ov")
}

// exchange posts the refresh grant. GitHub waives the client secret for GitHub App tokens
// minted by the device flow, which is what lets a gateway renew unattended without storing
// one. OAuth Apps get no such waiver, so their refresh is rejected here rather than fixed.
func (w *copilotTokenRefreshWorker) exchange(ctx context.Context, clientID, refreshToken string) (copilotRefreshResult, error) {
	var result copilotRefreshResult
	values := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return result, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := w.client.Do(request)
	if err != nil {
		return result, fmt.Errorf("github authorization unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("github authorization returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, copilotRefreshMaxBody+1))
	if err != nil || len(body) > copilotRefreshMaxBody {
		return result, fmt.Errorf("invalid authorization response")
	}
	if json.Unmarshal(body, &result) != nil {
		return result, fmt.Errorf("invalid authorization response")
	}
	return result, nil
}
