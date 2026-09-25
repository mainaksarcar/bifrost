package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fasthttp/router"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type copilotDeviceFlow struct {
	clientID   string
	deviceCode string
	expires    time.Time
	nextPoll   time.Time
	interval   int
	busy       bool
}

type CopilotDeviceHandler struct {
	client    *http.Client
	deviceURL string
	tokenURL  string
	mu        sync.Mutex
	flows     map[string]*copilotDeviceFlow
}

func NewCopilotDeviceHandler() *CopilotDeviceHandler {
	return &CopilotDeviceHandler{
		client:    &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		deviceURL: "https://github.com/login/device/code", tokenURL: "https://github.com/login/oauth/access_token",
		flows: make(map[string]*copilotDeviceFlow),
	}
}

func (h *CopilotDeviceHandler) RegisterRoutes(router *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	router.POST("/api/providers/github-copilot/device-login/initiate", lib.ChainMiddlewares(h.initiate, middlewares...))
	router.POST("/api/providers/github-copilot/device-login/poll", lib.ChainMiddlewares(h.poll, middlewares...))
}

func (h *CopilotDeviceHandler) exchange(endpoint string, values url.Values, result any) error {
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := h.client.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub authorization unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub authorization returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(body) > 65536 {
		return fmt.Errorf("invalid authorization response")
	}
	if json.Unmarshal(body, result) != nil {
		return fmt.Errorf("invalid authorization response")
	}
	return nil
}

func (h *CopilotDeviceHandler) initiate(ctx *fasthttp.RequestCtx) {
	var input struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(ctx.PostBody(), &input) != nil || strings.TrimSpace(input.ClientID) == "" || len(input.ClientID) > 256 {
		SendError(ctx, 400, "OAuth Client ID is required")
		return
	}
	h.mu.Lock()
	for id, flow := range h.flows {
		if time.Now().After(flow.expires) {
			delete(h.flows, id)
		}
	}
	if len(h.flows) >= 128 {
		h.mu.Unlock()
		SendError(ctx, 429, "too many device authorization flows")
		return
	}
	id := uuid.NewString()
	flow := &copilotDeviceFlow{clientID: strings.TrimSpace(input.ClientID), expires: time.Now().Add(15 * time.Minute)}
	h.flows[id] = flow
	h.mu.Unlock()
	var result struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	err := h.exchange(h.deviceURL, url.Values{"client_id": {flow.clientID}, "scope": {"read:user"}}, &result)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil || result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI != "https://github.com/login/device" || result.ExpiresIn <= 0 {
		delete(h.flows, id)
		SendError(ctx, 502, "could not initiate GitHub device authorization")
		return
	}
	flow.deviceCode = result.DeviceCode
	flow.interval = max(5, result.Interval)
	flow.expires = time.Now().Add(time.Duration(min(900, result.ExpiresIn)) * time.Second)
	flow.nextPoll = time.Now().Add(time.Duration(flow.interval) * time.Second)
	ctx.Response.Header.Set("Cache-Control", "no-store")
	SendJSON(ctx, map[string]any{"flow_id": id, "user_code": result.UserCode, "verification_uri": result.VerificationURI, "expires_in": min(900, result.ExpiresIn), "interval": flow.interval})
}

func (h *CopilotDeviceHandler) poll(ctx *fasthttp.RequestCtx) {
	var input struct {
		FlowID string `json:"flow_id"`
	}
	if json.Unmarshal(ctx.PostBody(), &input) != nil {
		SendError(ctx, 400, "flow_id is required")
		return
	}
	ctx.Response.Header.Set("Cache-Control", "no-store")
	h.mu.Lock()
	flow := h.flows[input.FlowID]
	if flow == nil || time.Now().After(flow.expires) {
		delete(h.flows, input.FlowID)
		h.mu.Unlock()
		SendJSON(ctx, map[string]string{"status": "expired"})
		return
	}
	if flow.busy || time.Now().Before(flow.nextPoll) {
		interval := flow.interval
		h.mu.Unlock()
		SendJSON(ctx, map[string]any{"status": "pending", "interval": interval})
		return
	}
	flow.busy = true
	h.mu.Unlock()
	var result struct {
		AccessToken string `json:"access_token"`
		// Present only when the app opts into expiring user tokens, which is the default
		// for GitHub Apps. OAuth Apps return neither and their tokens do not expire.
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	err := h.exchange(h.tokenURL, url.Values{"client_id": {flow.clientID}, "device_code": {flow.deviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &result)
	h.mu.Lock()
	defer h.mu.Unlock()
	flow.busy = false
	if err != nil {
		flow.nextPoll = time.Now().Add(time.Duration(flow.interval) * time.Second)
		SendError(ctx, 502, "GitHub authorization unavailable; retry")
		return
	}
	switch result.Error {
	case "authorization_pending", "slow_down":
		if result.Error == "slow_down" {
			flow.interval += 5
		}
		flow.nextPoll = time.Now().Add(time.Duration(flow.interval) * time.Second)
		SendJSON(ctx, map[string]any{"status": "pending", "interval": flow.interval})
	case "":
		delete(h.flows, input.FlowID)
		if result.AccessToken == "" {
			SendError(ctx, 502, "GitHub returned no user token")
			return
		}
		response := map[string]any{"status": "complete", "access_token": result.AccessToken}
		// Absolute rather than relative: the caller stores this, and a duration would
		// start counting again from whenever the write happened.
		if result.RefreshToken != "" {
			response["refresh_token"] = result.RefreshToken
		}
		if result.ExpiresIn > 0 {
			response["token_expires_at"] = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).Unix()
		}
		SendJSON(ctx, response)
	default:
		delete(h.flows, input.FlowID)
		SendJSON(ctx, map[string]string{"status": "error", "error": "Device authorization was denied or expired"})
	}
}
