package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return out, cleanError(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return out, ErrJoinCodeRejected
	case http.StatusTooManyRequests:
		return out, ErrRateLimited
	default:
		return out, fmt.Errorf("hub answered HTTP %d to registration", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.Secret == "" || out.Installation == "" {
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
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return out, cleanError(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return out, ErrUnauthorized
	default:
		return out, fmt.Errorf("hub answered HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return Whoami{}, errors.New("hub sent a malformed answer")
	}
	return out, nil
}
