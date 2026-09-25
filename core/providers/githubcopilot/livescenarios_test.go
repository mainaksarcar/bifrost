package githubcopilot_test

import (
	"os"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/internal/llmtests"
	"github.com/maximhq/bifrost/core/schemas"
)

// TestGithubCopilot is the entrypoint for `make test-core PROVIDER=githubcopilot`.
//
// It lives in the external test package because llmtests imports core, and core imports
// this provider. An internal test would close that cycle.
//
// Copilot has no free tier and no anonymous access, so this needs a real GitHub App with
// the Copilot Requests permission installed on an organization that allows Copilot
// requests from App installations. Without those the test skips, matching how every other
// live provider suite in this repo is gated.
func TestGithubCopilot(t *testing.T) {
	t.Parallel()

	if !hasCopilotCredentials(os.Getenv) {
		t.Skip("Skipping GitHub Copilot tests: set GITHUB_COPILOT_APP_ID (with " +
			"GITHUB_COPILOT_INSTALLATION_ID, GITHUB_COPILOT_REPOSITORY_ID and " +
			"GITHUB_COPILOT_PRIVATE_KEY) for server-to-server auth, or GITHUB_COPILOT_API_KEY " +
			"for a direct Copilot API token")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	// Model availability varies by plan tier and organization policy, so two operators with
	// valid credentials can see different catalogs. Confirm these against a live
	// GET /models before treating a failure here as a Bifrost bug.
	//
	// These are picked from GitHub's supported-models list rather than invented. gpt-4o is
	// deliberately not used: it no longer appears there and is not selectable in Copilot,
	// so it would fail for accounts whose credentials are perfectly valid.
	// https://docs.github.com/en/copilot/reference/ai-models/supported-models
	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.GithubCopilot,
		ChatModel: "gpt-5.5",
		Fallbacks: []schemas.Fallback{
			{Provider: schemas.GithubCopilot, Model: "gpt-5.5"},
			{Provider: schemas.GithubCopilot, Model: "gpt-5.4-mini"},
		},
		TextModel:      "gpt-5.5",
		EmbeddingModel: "", // exposed by Copilot, but not wired through this provider yet
		ReasoningModel: "gpt-5.5",
		VisionModel:    "gpt-5.5",
		Scenarios: llmtests.TestScenarios{
			// Copilot has no text completions endpoint; everything goes through chat.
			TextCompletion:        false,
			TextCompletionStream:  false,
			SimpleChat:            true,
			CompletionStream:      true,
			MultiTurnConversation: true,
			ToolCalls:             true,
			ToolCallsStreaming:    true,
			MultipleToolCalls:     true,
			End2EndToolCalling:    true,
			AutomaticFunctionCall: true,
			// Vision exercises the copilot-vision-request header, which Copilot requires on
			// image turns and rejects on text-only ones.
			ImageURL:        true,
			ImageBase64:     true,
			MultipleImages:  true,
			CompleteEnd2End: true,
			Embedding:       false,
			ListModels:      true,
			Reasoning:       false,
		},
	}

	if hasCopilotOAuthCredentials(os.Getenv) {
		// Without the build tag the SDK cannot start at all, so every scenario would fail
		// on environment rather than on behaviour. Skipping names the cause instead.
		if !inProcessBuild {
			t.Skip("Skipping GitHub Copilot OAuth tests: rebuild with -tags copilot_inprocess " +
				"and the bundled native runtime (see scripts/copilot-live-test.sh)")
		}
		// The SDK path accepts one text turn with an optional system message and refuses
		// tool, vision and n>1 contracts outright, so the scenarios below would assert
		// against a documented refusal rather than a regression. Model routing also goes
		// through the Copilot runtime, which resolves its own catalog.
		testConfig.ChatModel = copilotOAuthModel()
		testConfig.TextModel = testConfig.ChatModel
		testConfig.ReasoningModel = testConfig.ChatModel
		testConfig.VisionModel = testConfig.ChatModel
		testConfig.Fallbacks = nil
		testConfig.Scenarios = llmtests.TestScenarios{
			SimpleChat:       true,
			CompletionStream: true,
			ListModels:       true,
		}
	}

	t.Run("GithubCopilotTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// copilotOAuthModel lets the operator pin a model their Copilot plan actually serves;
// entitlements differ per account, so a hardcoded default would fail for valid credentials.
func copilotOAuthModel() string {
	if model := strings.TrimSpace(os.Getenv("GITHUB_COPILOT_OAUTH_MODEL")); model != "" {
		return model
	}
	return "gpt-5.5"
}

// hasCopilotCredentials reports whether enough credentials are present to run the live
// suite: an OAuth user token, a direct Copilot API token, or the complete GitHub App bundle.
//
// Completeness matters. A partial bundle does not fail fast, it reaches SetupTest with empty
// installation, repository or private-key values and fails deep inside the provider, where
// the error reads like a Bifrost bug rather than a missing secret.
func hasCopilotCredentials(getenv func(string) string) bool {
	if hasCopilotOAuthCredentials(getenv) {
		return true
	}
	if strings.TrimSpace(getenv("GITHUB_COPILOT_API_KEY")) != "" {
		return true
	}
	for _, name := range []string{
		"GITHUB_COPILOT_APP_ID",
		"GITHUB_COPILOT_INSTALLATION_ID",
		"GITHUB_COPILOT_REPOSITORY_ID",
		"GITHUB_COPILOT_PRIVATE_KEY",
	} {
		if strings.TrimSpace(getenv(name)) == "" {
			return false
		}
	}
	return true
}

// hasCopilotOAuthCredentials reports whether the device-login pair is present. Both halves
// are required: the token alone cannot identify which app minted it, and the client ID
// alone cannot authenticate.
func hasCopilotOAuthCredentials(getenv func(string) string) bool {
	return strings.TrimSpace(getenv("GITHUB_COPILOT_OAUTH_TOKEN")) != "" &&
		strings.TrimSpace(getenv("GITHUB_COPILOT_AUTH_CLIENT_ID")) != ""
}
