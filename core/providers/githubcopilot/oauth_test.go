package githubcopilot

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestOAuthCredentialRouting(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	key := schemas.Key{Value: *schemas.NewSecretVar("oauth-test"), GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "test-client"}}
	t.Setenv("BIFROST_COPILOT_SDK_URL", "http://127.0.0.1:8791")
	credentials, failure := resolveCredentials(ctx, key, nil, "https://api.individual.githubcopilot.com", nil)
	require.NotNil(t, failure, "OAuth credentials must never be forwarded to an HTTP bridge")
	require.Nil(t, credentials)
	for _, endpoint := range []string{"", "http://example.com", "http://127.0.0.1@example.com", "http://127.0.0.1:8791/path", "http://127.0.0.1:8791?token=yes"} {
		t.Setenv("BIFROST_COPILOT_SDK_URL", endpoint)
		_, failure := resolveCredentials(ctx, key, nil, "", nil)
		require.NotNil(t, failure)
		require.NotNil(t, failure.AllowFallbacks)
		require.False(t, *failure.AllowFallbacks)
	}
	key.GithubCopilotKeyConfig.AuthMode = "api_token"
	credentials, failure = resolveCredentials(ctx, key, nil, "https://api.individual.githubcopilot.com", nil)
	require.Nil(t, failure)
	require.Equal(t, "https://api.individual.githubcopilot.com", credentials.BaseURL)
}

func TestOAuthModelsAcrossKeys(t *testing.T) {
	t.Setenv("BIFROST_COPILOT_SDK_URL", "")
	provider, err := NewGithubCopilotProvider(&schemas.ProviderConfig{}, nil)
	require.NoError(t, err)
	var closed atomic.Int32
	provider.newSDKClient = func(_ context.Context, token string) (sdkClient, error) {
		return &fakeSDKClient{token: token, closed: &closed}, nil
	}
	var keys []schemas.Key
	for _, name := range []string{"invalid", "first", "second"} {
		keys = append(keys, schemas.Key{ID: name, Value: *schemas.NewSecretVar(name), Models: schemas.WhiteList{"*"}, GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "client"}})
	}
	result, failure := provider.ListModels(schemas.NewBifrostContext(context.Background(), time.Time{}), keys, &schemas.BifrostListModelsRequest{Unfiltered: true})
	require.Nil(t, failure)
	require.Len(t, result.Data, 2)
	require.EqualValues(t, 3, closed.Load())
	require.ElementsMatch(t, []string{"github-copilot/first-model", "github-copilot/second-model"}, []string{result.Data[0].ID, result.Data[1].ID})
}

type fakeSDKClient struct {
	token  string
	closed *atomic.Int32
}

func (client *fakeSDKClient) ListModels(context.Context) ([]schemas.Model, error) {
	if client.token == "invalid" {
		return nil, errors.New("upstream rejected credential gho_supersecrettoken")
	}
	return []schemas.Model{{ID: client.token + "-model"}}, nil
}

func (client *fakeSDKClient) Complete(_ context.Context, model, prompt, system string) (completion, error) {
	if model != "gpt-5-mini" || prompt != "Reply OK" || system != "Be brief" {
		return completion{}, errors.New("unexpected completion input")
	}
	// Mirrors the runtime resolving a routing alias to a concrete model.
	return completion{Content: "OK", Model: "resolved-model"}, nil
}

func (client *fakeSDKClient) Close() {
	client.closed.Add(1)
}

// Splits the reply so a caller can tell incremental delivery from one buffered blob.
func (client *fakeSDKClient) Stream(_ context.Context, model, prompt, system string, onDelta func(string)) (completion, error) {
	if model != "gpt-5-mini" || prompt != "Reply OK" || system != "Be brief" {
		return completion{}, errors.New("unexpected completion input")
	}
	onDelta("O")
	onDelta("K")
	return completion{Content: "OK", Model: "resolved-model"}, nil
}

func TestOAuthSDKCompletionAndCleanup(t *testing.T) {
	provider, err := NewGithubCopilotProvider(&schemas.ProviderConfig{}, nil)
	require.NoError(t, err)
	var closed atomic.Int32
	var opened []string
	provider.newSDKClient = func(_ context.Context, token string) (sdkClient, error) {
		opened = append(opened, token)
		return &fakeSDKClient{token: token, closed: &closed}, nil
	}
	key := schemas.Key{Value: *schemas.NewSecretVar("first"), GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "client"}}
	request := &schemas.BifrostChatRequest{Model: "gpt-5-mini", Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Be brief")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Reply OK")}},
	}}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	for _, token := range []string{"first", "rotated"} {
		key.Value = *schemas.NewSecretVar(token)
		response, failure := provider.ChatCompletion(ctx, key, request)
		require.Nil(t, failure)
		require.Equal(t, "OK", *response.Choices[0].Message.Content.ContentStr)
		require.Equal(t, schemas.GithubCopilot, response.ExtraFields.Provider)
		require.Nil(t, response.Usage)
		// The served model has to be reported, not the requested one echoed back.
		require.Equal(t, "resolved-model", response.Model)
	}
	require.Equal(t, []string{"first", "rotated"}, opened)
	require.EqualValues(t, 2, closed.Load())

	// Tuning knobs are dropped rather than refused, as other providers do with
	// options they cannot honor.
	request.Params = &schemas.ChatParameters{Temperature: schemas.Ptr(0.5), MaxCompletionTokens: schemas.Ptr(16)}
	response, failure := provider.ChatCompletion(ctx, key, request)
	require.Nil(t, failure)
	require.Equal(t, "OK", *response.Choices[0].Message.Content.ContentStr)
	require.Len(t, opened, 3)

	// Parameters that decide the reply's shape must still be refused, so a caller
	// expecting a tool call or parseable JSON never silently gets prose.
	for _, params := range []*schemas.ChatParameters{
		{Tools: []schemas.ChatTool{{Type: schemas.ChatToolTypeFunction}}},
		{ToolChoice: &schemas.ChatToolChoice{}},
		{ResponseFormat: schemas.Ptr(any(map[string]any{"type": "json_object"}))},
		{N: schemas.Ptr(2)},
	} {
		request.Params = params
		_, failure := provider.ChatCompletion(ctx, key, request)
		require.NotNil(t, failure, "contract-changing parameters must not be silently dropped")
	}
	require.Len(t, opened, 3)
}

func TestOAuthSDKFilteringAndErrors(t *testing.T) {
	provider, err := NewGithubCopilotProvider(&schemas.ProviderConfig{}, nil)
	require.NoError(t, err)
	var closed atomic.Int32
	provider.newSDKClient = func(_ context.Context, token string) (sdkClient, error) {
		return &fakeSDKClient{token: token, closed: &closed}, nil
	}
	key := schemas.Key{Value: *schemas.NewSecretVar("first"), Models: schemas.WhiteList{"other-model"}, GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "client"}}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	response, failure := provider.sdkListModels(ctx, key, &schemas.BifrostListModelsRequest{})
	require.Nil(t, failure)
	require.Empty(t, response.Data)
	key.Value = *schemas.NewSecretVar("invalid")
	_, failure = provider.sdkListModels(ctx, key, nil)
	require.NotNil(t, failure)
	// The cause has to reach the operator, but never the credential itself.
	require.Contains(t, failure.Error.Message, "upstream rejected credential")
	require.NotContains(t, failure.Error.Message, "gho_supersecrettoken")
	require.Contains(t, failure.Error.Message, "[REDACTED]")
	require.EqualValues(t, 2, closed.Load())
	provider.sdkSlots = make(chan struct{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, failure = provider.sdkListModels(schemas.NewBifrostContext(cancelled, time.Time{}), key, nil)
	require.NotNil(t, failure)
	require.EqualValues(t, 2, closed.Load())
}

func TestOAuthSDKChatCompletionStream(t *testing.T) {
	provider, err := NewGithubCopilotProvider(&schemas.ProviderConfig{}, nil)
	require.NoError(t, err)
	var closed atomic.Int32
	provider.newSDKClient = func(_ context.Context, token string) (sdkClient, error) {
		return &fakeSDKClient{token: token, closed: &closed}, nil
	}
	key := schemas.Key{Value: *schemas.NewSecretVar("first"), GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "client"}}
	request := &schemas.BifrostChatRequest{Model: "gpt-5-mini", Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Be brief")}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Reply OK")}},
	}}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	passthrough := func(_ *schemas.BifrostContext, response *schemas.BifrostResponse, failure *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return response, failure
	}

	stream, failure := provider.ChatCompletionStream(ctx, passthrough, func(context.Context) {}, key, request)
	require.Nil(t, failure)
	require.NotNil(t, stream)

	var content strings.Builder
	var deltas int
	var model, finishReason string
	var chunkIndices []int
	for chunk := range stream {
		require.Nil(t, chunk.BifrostError)
		require.NotNil(t, chunk.BifrostChatResponse)
		if chunk.BifrostChatResponse.Model != "" {
			model = chunk.BifrostChatResponse.Model
		}
		chunkIndices = append(chunkIndices, chunk.BifrostChatResponse.ExtraFields.ChunkIndex)
		for _, choice := range chunk.BifrostChatResponse.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			streamChoice := choice.ChatStreamResponseChoice
			if streamChoice == nil || streamChoice.Delta == nil || streamChoice.Delta.Content == nil {
				continue
			}
			content.WriteString(*streamChoice.Delta.Content)
			deltas++
		}
	}

	require.Equal(t, "OK", content.String())
	// More than one delta is the whole point: a single chunk would mean the reply
	// was buffered to completion and only then handed over.
	require.Greater(t, deltas, 1, "streaming must deliver incremental deltas")
	require.Equal(t, "resolved-model", model, "the served model must be reported, not the requested one")
	require.Equal(t, "stop", finishReason)
	// Consumers order chunks by index, so the synthesized final chunk must continue
	// the delta sequence rather than skip a value.
	require.Equal(t, []int{1, 2, 3}, chunkIndices)
	require.EqualValues(t, 1, closed.Load(), "the SDK client must be released once the stream ends")
}
