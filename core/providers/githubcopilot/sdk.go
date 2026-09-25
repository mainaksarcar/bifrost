package githubcopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/google/uuid"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

type sdkClient interface {
	ListModels(context.Context) ([]schemas.Model, error)
	Complete(context.Context, string, string, string) (completion, error)
	Stream(context.Context, string, string, string, func(string)) (completion, error)
	Close()
}

// sdkStreamDeltaBuffer decouples the runtime's event pump from downstream
// backpressure; a full buffer throttles the runtime rather than dropping text.
const sdkStreamDeltaBuffer = 256

// Model is what the runtime reports having run, which is not always what was asked
// for: routing aliases such as "auto" resolve to a concrete model.
type completion struct {
	Content string
	Model   string
	Usage   *schemas.BifrostLLMUsage
}

// addUsage folds one runtime usage event into the running total. A turn can report
// several model calls, and billing owes the sum of them rather than the last one.
func addUsage(total *schemas.BifrostLLMUsage, event *copilot.AssistantUsageData) *schemas.BifrostLLMUsage {
	if event == nil {
		return total
	}
	if total == nil {
		total = &schemas.BifrostLLMUsage{}
	}
	if event.InputTokens != nil {
		total.PromptTokens += int(*event.InputTokens)
	}
	if event.OutputTokens != nil {
		total.CompletionTokens += int(*event.OutputTokens)
	}
	if event.ReasoningTokens != nil {
		if total.CompletionTokensDetails == nil {
			total.CompletionTokensDetails = &schemas.ChatCompletionTokensDetails{}
		}
		total.CompletionTokensDetails.ReasoningTokens += int(*event.ReasoningTokens)
	}
	total.TotalTokens = total.PromptTokens + total.CompletionTokens
	return total
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

	var usageMu sync.Mutex
	var usage *schemas.BifrostLLMUsage
	unsubscribe := session.On(func(event copilot.SessionEvent) {
		if data, ok := event.Data.(*copilot.AssistantUsageData); ok {
			usageMu.Lock()
			usage = addUsage(usage, data)
			usageMu.Unlock()
		}
	})
	defer unsubscribe()

	event, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: prompt})
	if err != nil {
		return completion{}, err
	}
	if event != nil {
		if message, ok := event.Data.(*copilot.AssistantMessageData); ok && message.Content != "" {
			usageMu.Lock()
			total := usage
			usageMu.Unlock()
			return completion{Content: message.Content, Model: client.servedModel(ctx, session, model), Usage: total}, nil
		}
	}
	return completion{}, errors.New("empty SDK completion")
}

// Stream mirrors Complete but reports assistant text incrementally via onDelta.
// The returned completion still carries the served model and the full reply, so a
// runtime that declines to stream remains usable.
func (client *inProcessSDKClient) Stream(ctx context.Context, model, prompt, system string, onDelta func(string)) (completion, error) {
	session, err := client.client.CreateSession(ctx, &copilot.SessionConfig{
		Model:                 model,
		AvailableTools:        []string{},
		EnableConfigDiscovery: copilot.Bool(false),
		Streaming:             copilot.Bool(true),
		SystemMessage:         &copilot.SystemMessageConfig{Mode: "replace", Content: system},
		OnPermissionRequest: func(copilot.PermissionRequest, copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
			return &rpc.PermissionDecisionReject{}, nil
		},
	})
	if err != nil {
		return completion{}, err
	}
	defer session.Disconnect()

	// The runtime reports usage on its own event, off the goroutine that delivers
	// deltas, so the total needs guarding.
	var usageMu sync.Mutex
	var usage *schemas.BifrostLLMUsage

	unsubscribe := session.On(func(event copilot.SessionEvent) {
		switch data := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if data.DeltaContent != "" {
				onDelta(data.DeltaContent)
			}
		case *copilot.AssistantUsageData:
			usageMu.Lock()
			usage = addUsage(usage, data)
			usageMu.Unlock()
		}
	})
	defer unsubscribe()

	event, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: prompt})
	if err != nil {
		return completion{}, err
	}
	result := completion{Model: client.servedModel(ctx, session, model)}
	if event != nil {
		if message, ok := event.Data.(*copilot.AssistantMessageData); ok {
			result.Content = message.Content
		}
	}
	usageMu.Lock()
	result.Usage = usage
	usageMu.Unlock()
	return result, nil
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
	if strings.TrimSpace(key.Value.GetValue()) == "" || strings.TrimSpace(key.GithubCopilotKeyConfig.AuthClientID) == "" {
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

// prepareSDKChat validates the request against what SDK mode can currently honor
// and returns the user prompt, the system message to apply, and the tuning knobs
// being discarded so the caller can be told about them.
func (p *githubCopilotProvider) prepareSDKChat(request *schemas.BifrostChatRequest) (string, string, []string, *schemas.BifrostError) {
	if request == nil || request.Model == "" || len(request.Input) < 1 || len(request.Input) > 2 || len(request.RawRequestBody) > 0 {
		return "", "", nil, configurationError("github copilot: SDK mode currently accepts one text user message and an optional system message")
	}
	var dropped []string
	if request.Params != nil {
		if unsupported := contractChangingParams(request.Params); len(unsupported) > 0 {
			return "", "", nil, configurationError("github copilot: " + strings.Join(unsupported, ", ") + " not supported in SDK mode")
		}
		// The SDK's session API exposes no sampling or output-limit controls, so these
		// are dropped rather than refused, matching how other providers handle knobs
		// they cannot honor. Reported back on the response because dropping
		// max_tokens leaves the reply unbounded, which a log alone never tells the caller.
		if dropped = droppedParams(request.Params); len(dropped) > 0 && p.logger != nil {
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
			return "", "", nil, configurationError("github copilot: SDK mode currently accepts one text user message and an optional system message")
		}
		if expectedRole == schemas.ChatMessageRoleSystem {
			system = *message.Content.ContentStr
		}
	}
	return *request.Input[len(request.Input)-1].Content.ContentStr, system, dropped, nil
}

func (p *githubCopilotProvider) sdkChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	prompt, system, dropped, bErr := p.prepareSDKChat(request)
	if bErr != nil {
		return nil, bErr
	}
	started := time.Now()
	var result completion
	err := p.withSDK(ctx, key, func(ctx context.Context, client sdkClient) error {
		var err error
		result, err = client.Complete(ctx, request.Model, prompt, system)
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
		Usage:       result.Usage,
		ExtraFields: schemas.BifrostResponseExtraFields{Provider: p.GetProviderKey(), RequestType: schemas.ChatCompletionRequest, Latency: time.Since(started).Milliseconds(), DroppedCompatPluginParams: dropped},
	}, nil
}

func (p *githubCopilotProvider) sdkChatCompletionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	prompt, system, dropped, bErr := p.prepareSDKChat(request)
	if bErr != nil {
		return nil, bErr
	}

	responseChan := make(chan *schemas.BifrostStreamChunk, schemas.DefaultStreamBufferSize)
	messageID := "chatcmpl-" + uuid.NewString()
	created := int(time.Now().Unix())
	started := time.Now()

	// A Responses or Anthropic request reaches the SDK through ResponsesStream, which
	// falls back to chat. HTTP providers get the spread back into Responses events for
	// free inside the shared OpenAI streaming helper; the SDK path never touches it, so
	// without this the caller receives chat chunks on a Responses wire.
	var responsesState *schemas.ChatToResponsesStreamState
	if fallback, ok := ctx.Value(schemas.BifrostContextKeyIsResponsesToChatCompletionFallback).(bool); ok && fallback {
		responsesState = schemas.AcquireChatToResponsesStreamState()
	}

	go func() {
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		if responsesState != nil {
			defer schemas.ReleaseChatToResponsesStreamState(responsesState)
		}
		defer providerUtils.CloseStream(ctx, responseChan)

		chunkIndex := 0
		lastChunkTime := time.Now()
		servedModel := request.Model

		send := func(response *schemas.BifrostChatResponse) {
			if responsesState == nil {
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, response, nil, nil, nil, nil), responseChan, postHookSpanFinalizer)
				return
			}
			// One chat frame spreads into several Responses events.
			for _, converted := range response.ToBifrostResponsesStreamResponse(responsesState) {
				converted.ExtraFields.Provider = p.GetProviderKey()
				converted.ExtraFields.RequestType = schemas.ResponsesStreamRequest
				converted.ExtraFields.ChunkIndex = converted.SequenceNumber
				converted.ExtraFields.Latency = time.Since(lastChunkTime).Milliseconds()
				lastChunkTime = time.Now()
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetBifrostResponseForStreamResponse(nil, nil, converted, nil, nil, nil), responseChan, postHookSpanFinalizer)
			}
		}

		emit := func(text string) {
			delta := text
			// Pre-increment so the synthesized final chunk, which the shared helper
			// numbers chunkIndex+1, continues the sequence instead of skipping a value.
			chunkIndex++
			streamDelta := &schemas.ChatStreamResponseChoiceDelta{Content: &delta}
			// Real chat streams announce the role on the first chunk, and the Responses
			// spread keys response.created/in_progress off it, so a stream without it is
			// missing its opening lifecycle events.
			if chunkIndex == 1 {
				streamDelta.Role = schemas.Ptr(string(schemas.ChatMessageRoleAssistant))
			}
			response := &schemas.BifrostChatResponse{
				ID: messageID, Model: servedModel, Created: created, Object: "chat.completion.chunk",
				Choices: []schemas.BifrostResponseChoice{{
					ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
						Delta: streamDelta,
					},
				}},
				ExtraFields: schemas.BifrostResponseExtraFields{
					Provider:    p.GetProviderKey(),
					RequestType: schemas.ChatCompletionStreamRequest,
					ChunkIndex:  chunkIndex,
					Latency:     time.Since(lastChunkTime).Milliseconds(),
				},
			}
			lastChunkTime = time.Now()
			send(response)
		}

		// Usage is only known once the turn ends, so it is carried out here and attached
		// to the terminal chunk below.
		var streamUsage *schemas.BifrostLLMUsage

		err := p.withSDK(ctx, key, func(sdkCtx context.Context, client sdkClient) error {
			deltas := make(chan string, sdkStreamDeltaBuffer)
			var result completion
			var streamErr error
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				defer close(deltas)
				result, streamErr = client.Stream(sdkCtx, request.Model, prompt, system, func(text string) {
					if text == "" {
						return
					}
					select {
					case deltas <- text:
					case <-sdkCtx.Done():
					}
				})
			}()
			for text := range deltas {
				emit(text)
			}
			<-finished
			if streamErr != nil {
				return streamErr
			}
			if result.Model != "" {
				servedModel = result.Model
			}
			streamUsage = result.Usage
			// A runtime that completes the turn without emitting deltas still owes the
			// caller the reply, so fall back to the buffered content rather than
			// closing an empty stream.
			if chunkIndex == 0 && result.Content != "" {
				emit(result.Content)
			}
			return nil
		})

		ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
		if err != nil {
			providerUtils.ProcessAndSendBifrostError(ctx, postHookRunner, configurationError(err.Error()), responseChan, p.logger, postHookSpanFinalizer)
			return
		}

		final := providerUtils.CreateBifrostChatCompletionChunkResponse(messageID, nil, schemas.Ptr("stop"), chunkIndex, servedModel, created)
		final.ExtraFields.Provider = p.GetProviderKey()
		final.ExtraFields.RequestType = schemas.ChatCompletionStreamRequest
		final.ExtraFields.Latency = time.Since(started).Milliseconds()
		// Usage arrives once the turn is done, so the terminal chunk is the only one that
		// can carry it - and governance reads it as free until it does.
		final.Usage = streamUsage
		// Same reason as the unary path: an unhonored output cap the caller cannot see
		// is indistinguishable from one that was applied.
		final.ExtraFields.DroppedCompatPluginParams = dropped
		// The terminal chunk carries finish_reason, which is what the Responses spread
		// turns into the completed event, so it has to go through the same path.
		send(final)
	}()

	return responseChan, nil
}
