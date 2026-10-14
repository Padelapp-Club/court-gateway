package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// ErrJoinCodeRejected: the hub does not accept the code (unknown, used or
// expired; the hub deliberately does not say which).
var ErrJoinCodeRejected = errors.New("join code rejected")

// ErrRateLimited: the hub refuses registrations from this address for a
// while after too many failed ones.
var ErrRateLimited = errors.New("too many failed registrations from this address")

// FetchHubKey reads the hub's public signing key, unauthenticated: setup
// checks it out of band before it spends a join code.
func FetchHubKey(ctx context.Context, rt http.RoundTripper, hubURL string) (ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+PathHubKey, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		HubKey string `json:"hub_key"`
	}
	status, err := exchange(hubClient(rt).Do, req, 4096, &out)
	switch {
	case status == 0:
		return nil, err
	case status != http.StatusOK:
		return nil, answerError(status, "the hub key")
	}
	key, kerr := core.ParseHubKey(out.HubKey)
	if err != nil || kerr != nil {
		return nil, errors.New("hub sent no valid signing key")
	}
	return key, nil
}

// Register exchanges a join code for the gateway secret. Nothing of the
// answer may be used before RegisterResponse.Verify with the key checked
// out of band (FetchHubKey): an interceptor of the TLS could have written
// any field.
func Register(ctx context.Context, rt http.RoundTripper, hubURL, code, version string) (RegisterResponse, error) {
	var out RegisterResponse
	body, _ := json.Marshal(RegisterRequest{Code: strings.TrimSpace(code), Version: version})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+PathRegister, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, err := exchange(hubClient(rt).Do, req, 64<<10, &out)
	switch {
	case status == 0:
		return out, err
	case status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone:
		return out, ErrJoinCodeRejected
	case status == http.StatusTooManyRequests:
		return out, ErrRateLimited
	case status != http.StatusOK:
		return out, answerError(status, "registration")
	case err != nil || out.Secret == "" || !core.ValidID(out.Installation):
		return RegisterResponse{}, errors.New("hub sent a malformed registration answer")
	}
	return out, nil
}

// Whoami asks the hub how it sees this gateway and takes the hub's signed
// time from the answer, so what the hub signs is checked against its
// clock (doctor).
func (c *Client) Whoami(ctx context.Context) (Whoami, error) {
	var out Whoami
	req, err := c.newRequest(ctx, http.MethodGet, PathWhoami, nil)
	if err != nil {
		return out, err
	}
	status, err := exchange(c.doTimed, req, 64<<10, &out)
	switch {
	case status == 0:
		return out, err
	case status != http.StatusOK:
		return out, answerError(status, "whoami")
	case err != nil:
		return Whoami{}, errors.New("hub sent a malformed answer")
	}
	return out, nil
}

// exchange sends req and, when the hub answers 200, decodes at most limit
// bytes of JSON into out. status is 0 when no answer came (err says why);
// otherwise err is a decoding failure of a 200 answer.
func exchange(do func(*http.Request) (*http.Response, error), req *http.Request, limit int64, out any) (status int, err error) {
	resp, err := do(req)
	if err != nil {
		return 0, cleanError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		err = json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out)
	}
	return resp.StatusCode, err
}
