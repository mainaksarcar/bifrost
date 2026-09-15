package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestCopilotDeviceClientBinding(t *testing.T) {
	handler := NewCopilotDeviceHandler()
	var clients []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.NoError(t, request.ParseForm())
		clients = append(clients, request.Form.Get("client_id"))
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/device" {
			_, _ = writer.Write([]byte(`{"device_code":"private-code","user_code":"USER-CODE","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		} else {
			_, _ = writer.Write([]byte(`{"access_token":"test-user-token"}`))
		}
	}))
	defer server.Close()
	handler.deviceURL = server.URL + "/device"
	handler.tokenURL = server.URL + "/token"
	var ctx fasthttp.RequestCtx
	ctx.Request.SetBodyString(`{}`)
	handler.initiate(&ctx)
	require.Equal(t, 400, ctx.Response.StatusCode())
	ctx.Response.Reset()
	ctx.Request.SetBodyString(`{"client_id":"custom-client"}`)
	handler.initiate(&ctx)
	require.Equal(t, 200, ctx.Response.StatusCode())
	require.NotContains(t, string(ctx.Response.Body()), "private-code")
	var response map[string]any
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &response))
	flowID := response["flow_id"].(string)
	handler.flows[flowID].nextPoll = time.Time{}
	body, _ := json.Marshal(map[string]string{"flow_id": flowID, "client_id": "wrong-client"})
	ctx.Request.SetBody(body)
	ctx.Response.Reset()
	handler.poll(&ctx)
	require.Equal(t, 200, ctx.Response.StatusCode())
	require.Contains(t, string(ctx.Response.Body()), "test-user-token")
	require.Equal(t, []string{"custom-client", "custom-client"}, clients)
	require.Empty(t, handler.flows)
}

func TestCopilotKeyAuthModes(t *testing.T) {
	for _, mode := range []string{"oauth", "api_token"} {
		key := schemas.Key{Value: *schemas.NewSecretVar("test-token"), GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: mode, OAuthClientID: "client"}}
		require.NoError(t, validateProviderKeyURL(schemas.GithubCopilot, key))
		key.GithubCopilotKeyConfig.AppID = *schemas.NewSecretVar("app")
		require.Error(t, validateProviderKeyURL(schemas.GithubCopilot, key))
	}
	require.Error(t, validateProviderKeyURL(schemas.GithubCopilot, schemas.Key{Value: *schemas.NewSecretVar("test"), GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth"}}))
}
