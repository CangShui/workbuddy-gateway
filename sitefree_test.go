package main

import "testing"

func TestSiteKnownFreeSkipsUnusableAccounts(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	accountMu.Lock()
	oldAccounts := accounts
	accountMu.Unlock()
	modelsMu.Lock()
	oldCatalogs, oldProbes := catalogModels, modelProbes
	catalogModels = map[string][]catalogModel{}
	modelProbes = map[string]modelPriceProbe{}
	modelsMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
		modelsMu.Lock()
		catalogModels, modelProbes = oldCatalogs, oldProbes
		modelsMu.Unlock()
	})
	free := map[string]*modelRuntimeState{model: {CostClass: modelCostFree}}
	for _, tc := range []struct {
		name string
		acc  *Account
		want bool
	}{
		{"nil_account", nil, false},
		{"disabled_marker", &Account{Disabled: true, Edition: "cn"}, false},
		{"missing_auth", &Account{Edition: "cn", ModelStates: free}, false},
		{"empty_token", &Account{Auth: &StoredAuth{Edition: "cn"}, ModelStates: free}, false},
		{"disabled_free_history", &Account{Disabled: true, Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}, ModelStates: free}, false},
		{"active_free", &Account{Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "test"}}, ModelStates: free}, true},
		{"other_site", &Account{Auth: &StoredAuth{Edition: "intl", Auth: StoredTokens{AccessToken: "test"}}, ModelStates: free}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountMu.Lock()
			accounts = []*Account{nil, {Disabled: true, Edition: "cn"}, tc.acc}
			accountMu.Unlock()
			if got := siteKnownFree("cn", model); got != tc.want {
				t.Fatalf("siteKnownFree=%v want=%v", got, tc.want)
			}
			// The scheduler must remain usable after checking marker-only accounts.
			accountMu.Lock()
			accountMu.Unlock()
		})
	}
}
