package auth

import "testing"

func TestZedExchangeCredentialIsRefreshable(t *testing.T) {
	credential := map[string]any{"access_token": "account-credential"}
	for _, tc := range []struct {
		name string
		auth *Auth
		want bool
	}{
		{"zed", &Auth{Provider: "zed", Metadata: map[string]any{"credential": credential, "organization_id": "org"}}, true},
		{"missing organization", &Auth{Provider: "zed", Metadata: map[string]any{"credential": credential}}, false},
		{"blank organization", &Auth{Provider: "zed", Metadata: map[string]any{"credential": credential, "organization_id": " "}}, false},
		{"missing credential", &Auth{Provider: "zed", Metadata: map[string]any{"organization_id": "org"}}, false},
		{"string credential", &Auth{Provider: "zed", Metadata: map[string]any{"credential": "x", "organization_id": "org"}}, false},
		{"non-zed", &Auth{Provider: "codex", Metadata: map[string]any{"credential": credential, "organization_id": "org"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authHasRefreshCredential(tc.auth); got != tc.want {
				t.Fatalf("refresh eligible = %v, want %v", got, tc.want)
			}
		})
	}
}
