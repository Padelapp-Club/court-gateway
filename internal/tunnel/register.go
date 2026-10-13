package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrJoinCodeRejected: the hub does not accept the code (unknown, used or
// expired; the hub deliberately does not say which).
var ErrJoinCodeRejected = errors.New("join code rejected")

// ErrRateLimited: the hub refuses registrations from this address for a
// while after too many failed ones.
var ErrRateLimited = errors.New("too many failed registrations from this address")

// Register exchanges a join code for the gateway secret.
func Register(ctx context.Context, rt http.RoundTripper, hubURL, code, version string) (RegisterResponse, error) {
	var out RegisterResponse
	body, _ := json.Marshal(RegisterRequest{Code: strings.TrimSpace(code), Version: version})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+PathRegister, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, err := exchange(hubClient(rt), req, 64<<10, &out)
	switch {
	case status == 0:
		return out, err
	case status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone:
		return out, ErrJoinCodeRejected
	case status == http.StatusTooManyRequests:
		return out, ErrRateLimited
	case status != http.StatusOK:
		return out, answerError(status, "registration")
	case err != nil || out.Secret == "" || out.Installation == "":
		return RegisterResponse{}, errors.New("hub sent a malformed registration answer")
	}
	return out, nil
}

// FetchWhoami asks the hub how it sees this gateway.
func FetchWhoami(ctx context.Context, rt http.RoundTripper, hubURL, secret string) (Whoami, error) {
	var out Whoami
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+PathWhoami, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	status, err := exchange(hubClient(rt), req, 64<<10, &out)
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
func exchange(hc *http.Client, req *http.Request, limit int64, out any) (status int, err error) {
	resp, err := hc.Do(req)
	if err != nil {
		return 0, cleanError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		err = json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out)
	}
	return resp.StatusCode, err
}
