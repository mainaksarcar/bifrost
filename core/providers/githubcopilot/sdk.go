package githubcopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
)

type sdkClient interface {
	ListModels(context.Context) ([]schemas.Model, error)
	Complete(context.Context, string, string, string) (completion, error)
	Close()
}

// Model is what the runtime reports having run, which is not always what was asked
// for: routing aliases such as "auto" resolve to a concrete model.
type completion struct {
	Content string
	Model   string
}

type sdkClientFactory func(context.Context, string) (sdkClient, error)

type inProcessSDKClient struct {
	client    *copilot.Client
	directory string
}

func isOAuthKey(key schemas.Key) bool {
	return key.GithubCopilotKeyConfig != nil && key.GithubCopilotKeyConfig.AuthMode == "oauth"
}

// contractChangingParams lists parameters that decide the shape of the reply rather
// than tune it. Silently dropping these would hand back a plain single completion to
// a caller expecting a tool call, parseable JSON, audio, or n candidates.
func contractChangingParams(params *schemas.ChatParameters) []string {
	var unsupported []string
	if len(params.Tools) > 0 {
		unsupported = append(unsupported, "tools")
	}
	if params.ToolChoice != nil {
		unsupported = append(unsupported, "tool_choice")
	}
	if params.ResponseFormat != nil {
		unsupported = append(unsupported, "response_format")
	}
	if params.N != nil && *params.N != 1 {
		unsupported = append(unsupported, "n")
	}
	if params.Audio != nil || len(params.Modalities) > 0 {
		unsupported = append(unsupported, "audio/modalities")
	}
	if len(params.MCPServers) > 0 {
		unsupported = append(unsupported, "mcp_servers")
	}
	if params.WebSearchOptions != nil {
		unsupported = append(unsupported, "web_search_options")
	}
	return unsupported
}

// droppedParams names the tuning knobs being discarded, for the operator-facing log.
func droppedParams(params *schemas.ChatParameters) []string {
	var dropped []string
	for _, knob := range []struct {
		name string
		set  bool
	}{
		{"max_completion_tokens", params.MaxCompletionTokens != nil},
		{"temperature", params.Temperature != nil},
		{"top_p", params.TopP != nil},
		{"top_k", params.TopK != nil},
		{"stop", len(params.Stop) > 0},
		{"seed", params.Seed != nil},
		{"frequency_penalty", params.FrequencyPenalty != nil},
		{"presence_penalty", params.PresencePenalty != nil},
		{"logit_bias", params.LogitBias != nil},
		{"logprobs", params.LogProbs != nil},
		{"reasoning", params.Reasoning != nil},
		{"service_tier", params.ServiceTier != nil},
		{"verbosity", params.Verbosity != nil},
	} {
		if knob.set {
			dropped = append(dropped, knob.name)
		}
	}
	return dropped
}

// credentialPattern is a backstop for tokens the SDK may echo back inside an error.
var credentialPattern = regexp.MustCompile(`(gho_|ghu_|ghp_|github_pat_)[A-Za-z0-9_]+`)

func sanitizeSDKError(err error, token string) string {
	message := err.Error()
	if strings.TrimSpace(token) != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return credentialPattern.ReplaceAllString(message, "[REDACTED]")
}

func newInProcessSDKClient(ctx context.Context, token string) (sdkClient, error) {
	directory, err := os.MkdirTemp("", "bifrost-copilot-")
	if err != nil {
		return nil, errors.New("github copilot: cannot create isolated SDK storage")
	}
	client := &inProcessSDKClient{
		directory: directory,
		client: copilot.NewClient(&copilot.ClientOptions{
			Connection:      copilot.InProcessConnection{},
			Mode:            copilot.ModeEmpty,
			BaseDirectory:   directory,
			GitHubToken:     token,
			UseLoggedInUser: copilot.Bool(false),
			LogLevel:        "none",
		}),
	}
	if err := client.client.Start(ctx); err != nil {
		startErr := fmt.Errorf("in-process SDK startup failed (build with copilot_inprocess and the bundled native runtime): %w", err)
		client.Close()
		return nil, startErr
	}
	return client, nil
}

func (client *inProcessSDKClient) Close() {
	client.client.ForceStop()
	_ = os.RemoveAll(client.directory)
}

func (client *inProcessSDKClient) ListModels(ctx context.Context) ([]schemas.Model, error) {
	models, err := client.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]schemas.Model, 0, len(models))
	for _, model := range models {
		result = append(result, schemas.Model{ID: model.ID, Name: schemas.Ptr(model.Name)})
	}
	return result, nil
}

func (client *inProcessSDKClient) Complete(ctx context.Context, model, prompt, system string) (completion, error) {
	session, err := client.client.CreateSession(ctx, &copilot.SessionConfig{
		Model:                 model,
		AvailableTools:        []string{},
		EnableConfigDiscovery: copilot.Bool(false),
		SystemMessage:         &copilot.SystemMessageConfig{Mode: "replace", Content: system},
		OnPermissionRequest: func(copilot.PermissionRequest, copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionReject{}, nil
		},
	})
	if err != nil {
		return completion{}, err
	}
	defer session.Disconnect()
	event, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: prompt})
	if err != nil {
		return completion{}, err
	}
	if event != nil {
		if message, ok := event.Data.(*copilot.AssistantMessageData); ok && message.Content != "" {
			return completion{Content: message.Content, Model: client.servedModel(ctx, session, model)}, nil
		}
	}
	return completion{}, errors.New("empty SDK completion")
}

// servedModel falls back to the requested model: reporting is not worth failing a
// completed turn over.
func (client *inProcessSDKClient) servedModel(ctx context.Context, session *copilot.Session, requested string) string {
	if session.RPC == nil || session.RPC.Model == nil {
		return requested
	}
	current, err := session.RPC.Model.GetCurrent(ctx)
	if err != nil || current == nil || current.ModelID == nil || *current.ModelID == "" {
		return requested
	}
	return *current.ModelID
}

func (p *githubCopilotProvider) withSDK(ctx context.Context, key schemas.Key, operation func(context.Context, sdkClient) error) error {
	if strings.TrimSpace(key.Value.GetValue()) == "" || strings.TrimSpace(key.GithubCopilotKeyConfig.OAuthClientID) == "" {
		return errors.New("github copilot: OAuth mode requires a client ID and a saved user token")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	select {
	case p.sdkSlots <- struct{}{}:
		defer func() { <-p.sdkSlots }()
	case <-ctx.Done():
		return errors.New("github copilot: SDK request cancelled while waiting for capacity")
	}
	client, err := p.newSDKClient(ctx, key.Value.GetValue())
	if err != nil {
		return fmt.Errorf("github copilot: in-process SDK startup failed: %s", sanitizeSDKError(err, key.Value.GetValue()))
	}
	defer client.Close()
	if err := operation(ctx, client); err != nil {
		return fmt.Errorf("github copilot: in-process SDK request failed: %s", sanitizeSDKError(err, key.Value.GetValue()))
	}
	return nil
}

func (p *githubCopilotProvider) sdkListModels(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	var models []schemas.Model
	err := p.withSDK(ctx, key, func(ctx context.Context, client sdkClient) error {
		var err error
		models, err = client.ListModels(ctx)
		return err
	})
	if err != nil {
		return nil, configurationError(err.Error())
	}
	result := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}}
	for _, model := range models {
		if request != nil && request.Unfiltered || key.Models.IsAllowed(model.ID) {
			model.ID = string(p.GetProviderKey()) + "/" + model.ID
			result.Data = append(result.Data, model)
		}
	}
	result.ExtraFields.Provider = p.GetProviderKey()
	result.ExtraFields.RequestType = schemas.ListModelsRequest
	return result, nil
}

func (p *githubCopilotProvider) sdkChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	if request == nil || request.Model == "" || len(request.Input) < 1 || len(request.Input) > 2 || len(request.RawRequestBody) > 0 {
		return nil, configurationError("github copilot: SDK mode currently accepts one text user message and an optional system message")
	}
	if request.Params != nil {
		if unsupported := contractChangingParams(request.Params); len(unsupported) > 0 {
			return nil, configurationError("github copilot: " + strings.Join(unsupported, ", ") + " not supported in SDK mode")
		}
		// The SDK's session API exposes no sampling or output-limit controls, so these
		// are dropped rather than refused, matching how other providers handle knobs
		// they cannot honor. Logged because dropping max_tokens leaves output unbounded.
		if dropped := droppedParams(request.Params); len(dropped) > 0 && p.logger != nil {
			p.logger.Warn("github copilot: ignoring unsupported parameters in SDK mode: " + strings.Join(dropped, ", "))
		}
	}
	system := "Respond briefly. Do not use tools."
	for index, message := range request.Input {
		expectedRole := schemas.ChatMessageRoleUser
		if len(request.Input) == 2 && index == 0 {
			expectedRole = schemas.ChatMessageRoleSystem
		}
		if message.Role != expectedRole || message.Content == nil || message.Content.ContentStr == nil || len(message.Content.ContentBlocks) > 0 || message.Name != nil || message.ChatToolMessage != nil || message.ChatAssistantMessage != nil {
			return nil, configurationError("github copilot: SDK mode currently accepts one text user message and an optional system message")
		}
		if expectedRole == schemas.ChatMessageRoleSystem {
			system = *message.Content.ContentStr
		}
	}
	started := time.Now()
	var result completion
	err := p.withSDK(ctx, key, func(ctx context.Context, client sdkClient) error {
		var err error
		result, err = client.Complete(ctx, request.Model, *request.Input[len(request.Input)-1].Content.ContentStr, system)
		return err
	})
	if err != nil {
		return nil, configurationError(err.Error())
	}
	content := result.Content
	model := result.Model
	if model == "" {
		model = request.Model
	}
	return &schemas.BifrostChatResponse{
		ID: "chatcmpl-" + uuid.NewString(), Model: model, Created: int(time.Now().Unix()), Object: "chat.completion",
		Choices: []schemas.BifrostResponseChoice{{
			Index: 0, FinishReason: schemas.Ptr("stop"),
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &content}},
			},
		}},
		ExtraFields: schemas.BifrostResponseExtraFields{Provider: p.GetProviderKey(), RequestType: schemas.ChatCompletionRequest, Latency: time.Since(started).Milliseconds()},
	}, nil
}
