package providers

import "testing"

func TestClaudeRequiresFirstPartySubscription(t *testing.T) {
	for _, raw := range []string{`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"api_key"}`, `{"loggedIn":true,"apiProvider":"thirdParty","authMethod":"oauth_token"}`, `{"loggedIn":false,"apiProvider":"firstParty","authMethod":"oauth_token"}`, `ok`} {
		if claudeSubscription(raw) {
			t.Fatal("契約アカウントでない認証を受理した")
		}
	}
	if !claudeSubscription(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`) {
		t.Fatal("契約認証を拒否した")
	}
}

func TestRealClaudeContractStatus(t *testing.T) {
	if !claudeSubscription(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"claude.ai"}`) {
		t.Fatal("実CLIの契約認証表記を拒否しました")
	}
}
