package schemas

import (
	"encoding/json"
	"testing"
)

func TestGithubCopilotOAuthConfigRoundTrip(t *testing.T) {
	original := GithubCopilotKeyConfig{AuthMode: "oauth", OAuthClientID: "test-client"}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored GithubCopilotKeyConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.AuthMode != original.AuthMode || restored.OAuthClientID != original.OAuthClientID {
		t.Fatal("OAuth configuration was lost during JSON round trip")
	}
}
