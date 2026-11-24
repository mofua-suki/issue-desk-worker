package providers

import (
	"encoding/json"
	"strings"
)

func claudeSubscription(output string) bool {
	var auth struct {
		LoggedIn bool   `json:"loggedIn"`
		Provider string `json:"apiProvider"`
		Method   string `json:"authMethod"`
	}
	if json.Unmarshal([]byte(output), &auth) != nil {
		return false
	}
	method := strings.ToLower(auth.Method)
	return auth.LoggedIn && auth.Provider == "firstParty" && (method == "oauth_token" || method == "claude_ai" || method == "claude.ai" || method == "oauth")
}
