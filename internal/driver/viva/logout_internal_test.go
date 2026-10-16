package viva

import "testing"

// The logout endpoint is the token endpoint's neighbour, Keycloak's or a
// test double's; a token endpoint of another shape has none.
func TestLogoutURLOf(t *testing.T) {
	for in, want := range map[string]string{
		DefaultAuthURL:                      "https://kc.vivacrm.ru/realms/prod/protocol/openid-connect/logout",
		"http://127.0.0.1:8080/token":       "http://127.0.0.1:8080/logout",
		"https://kc.example.test/token?x=1": "https://kc.example.test/logout",
		"https://kc.example.test/auth":      "",
	} {
		if got := logoutURLOf(in); got != want {
			t.Errorf("logoutURLOf(%s) = %q, want %q", in, got, want)
		}
	}
}

// The credentials' key tells fields apart: a field boundary moved does not
// give the same key.
func TestCredentialKeySeparatesFields(t *testing.T) {
	a := &client{authURL: DefaultAuthURL, username: "user\x00segment", password: "tail"}
	b := &client{authURL: DefaultAuthURL, username: "user", password: "segment\x00tail"}
	if a.credentialKey() == b.credentialKey() {
		t.Fatal("two credentials share a key")
	}
}
