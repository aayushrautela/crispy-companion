// Package grant speaks crispy-server's RFC 8628 Device Authorization Grant
// (the flow already powering TV login) on behalf of the companion.
//
// The companion is treated exactly like a TV: an untrusted, inputless
// device that a logged-in browser approves. No Supabase session token and
// no user Bearer ever crosses the browser↔companion boundary; the only
// long-lived credential the companion holds is the server-minted,
// device-scoped cp_pat_, which travels TLS-direct server→companion.
//
// Server envelopes: success {data, meta:{requestId}}, errors
// {error:{code,message,category,retryable,requestId,details}}.
// Error values are safe to surface (they never contain tokens); response
// bodies on the poll path are never included in errors (they may contain
// the plaintext PAT after approval).
package grant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ClientID is the RFC 8628 client identifier for the companion. It must be
// present in crispy-server's DEVICE_AUTHORIZATION_CLIENT_IDS; until the
// server adds it, Authorize fails with ErrUnknownClient (fail-closed).
const ClientID = "crispy-companion"

// Server-side error codes we map explicitly.
const (
	codeInvalidClientID = "invalid_client_id"
	codeRateLimited     = "device_authorization_rate_limited"
)

var (
	// ErrUnknownClient means the server rejected our client_id. Fix: add
	// "crispy-companion" to DEVICE_AUTHORIZATION_CLIENT_IDS server-side.
	ErrUnknownClient = errors.New("server does not recognize client crispy-companion")
	// ErrRateLimited means the server throttled authorization creation.
	ErrRateLimited = errors.New("server rate-limited device authorization")
	// ErrPATDead means the stored cp_pat_ is revoked/expired/unknown.
	ErrPATDead = errors.New("device token revoked or expired")
)

// Authorization is the result of POST /v1/auth/device/authorize (HTTP 201).
type Authorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
	Interval                time.Duration
}

// Approved is the approved outcome of POST /v1/auth/device/token.
type Approved struct {
	PlaintextToken string // cp_pat_ — store 0600, never log
	DeviceID       string
	UserID         string
	Email          string
}

// Outcome is one poll result.
type Outcome struct {
	Status   string // authorization_pending | slow_down | approved | access_denied | expired_token
	Interval time.Duration
	Approved *Approved
}

// Client talks to crispy-server. HTTP should carry a Timeout.
type Client struct {
	ServerURL string
	HTTP      *http.Client
}

// New builds a client with a 10s default timeout.
func New(serverURL string) *Client {
	return &Client{
		ServerURL: serverURL,
		HTTP: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: preserveRequestOnRedirect,
		},
	}
}

// preserveRequestOnRedirect replays the original method and body when the
// server answers with a redirect.
//
// net/http rewrites POST to GET on 301/302/303 and drops the body. A Crispy
// deployment behind an nginx http→https redirect therefore never sees the
// device-grant POST and answers "must have required property 'clientId'"
// instead of a device code. Replaying the request keeps the grant working
// across the redirect and keeps the poll — which carries the device code and
// returns the cp_pat_ — intact.
func preserveRequestOnRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	orig := via[0]
	if orig == nil || req.Method == orig.Method {
		return nil // 307/308 already preserved method and body
	}
	if orig.GetBody == nil {
		return nil // nothing to replay (e.g. GET or HEAD)
	}
	body, err := orig.GetBody()
	if err != nil {
		return err
	}
	req.Method = orig.Method
	req.Body = body
	req.ContentLength = orig.ContentLength
	return nil
}

type envelope struct {
	Data json.RawMessage `json:"data"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Authorize requests device/user codes (public endpoint, no credentials).
func (c *Client) Authorize(ctx context.Context, deviceName, claimedDeviceID string) (Authorization, error) {
	body, _ := json.Marshal(map[string]any{
		"clientId":   ClientID,
		"deviceName": deviceName,
		"deviceId":   claimedDeviceID,
	})
	var out struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	status, code, err := c.post(ctx, "/v1/auth/device/authorize", body, &out)
	if err != nil {
		return Authorization{}, err
	}
	if status == http.StatusTooManyRequests || code == codeRateLimited {
		return Authorization{}, ErrRateLimited
	}
	if status == http.StatusBadRequest && code == codeInvalidClientID {
		return Authorization{}, ErrUnknownClient
	}
	if status != http.StatusCreated {
		return Authorization{}, fmt.Errorf("device authorize: unexpected status %d", status)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return Authorization{}, errors.New("device authorize: incomplete response")
	}
	interval := time.Duration(out.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	return Authorization{
		DeviceCode:              out.DeviceCode,
		UserCode:                out.UserCode,
		VerificationURI:         out.VerificationURI,
		VerificationURIComplete: out.VerificationURIComplete,
		ExpiresIn:               time.Duration(out.ExpiresIn) * time.Second,
		Interval:                interval,
	}, nil
}

// Poll exchanges the device code once (public endpoint, no credentials).
// Callers must honor Outcome.Interval and back off on slow_down.
func (c *Client) Poll(ctx context.Context, deviceCode string) (Outcome, error) {
	body, _ := json.Marshal(map[string]string{"deviceCode": deviceCode})
	var out struct {
		Status         string `json:"status"`
		Interval       int    `json:"interval"`
		PlaintextToken string `json:"plaintextToken"`
		DeviceID       string `json:"deviceId"`
		User           struct {
			ID    string  `json:"id"`
			Email *string `json:"email"`
		} `json:"user"`
	}
	status, _, err := c.post(ctx, "/v1/auth/device/token", body, &out)
	if err != nil {
		return Outcome{}, err
	}
	if status != http.StatusOK {
		return Outcome{}, fmt.Errorf("device token poll: unexpected status %d", status)
	}
	oc := Outcome{Status: out.Status}
	switch out.Status {
	case "slow_down":
		oc.Interval = time.Duration(out.Interval) * time.Second
		if oc.Interval < 5*time.Second {
			oc.Interval = 10 * time.Second
		}
	case "approved":
		if out.PlaintextToken == "" || out.DeviceID == "" {
			return Outcome{}, errors.New("device token poll: approved without token")
		}
		email := ""
		if out.User.Email != nil {
			email = *out.User.Email
		}
		oc.Approved = &Approved{
			PlaintextToken: out.PlaintextToken,
			DeviceID:       out.DeviceID,
			UserID:         out.User.ID,
			Email:          email,
		}
	case "authorization_pending", "access_denied", "expired_token":
		// Terminal/non-terminal states need no extra fields.
	default:
		return Outcome{}, fmt.Errorf("device token poll: unknown status %q", out.Status)
	}
	return oc, nil
}

// Revalidate checks whether the stored cp_pat_ is still alive by calling
// GET /v1/auth/devices with it as Bearer.
//
// The PAT authenticates as actor type "pat", while that route requires a
// user *session* actor, so a *live* PAT gets 403 here. Only 401 (unknown /
// revoked / expired hash) means death. Network errors are returned so the
// caller can fail open for existing tokens (offline keeps working).
func (c *Client) Revalidate(ctx context.Context, pat string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ServerURL+"/v1/auth/devices", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "crispy-companion/1")
	// Never log the PAT; it travels only in this header.
	req.Header.Set("Authorization", "Bearer "+pat)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrPATDead
	default:
		return nil // 200 alive; 403 alive-but-not-a-session; anything else inconclusive
	}
}

func (c *Client) post(ctx context.Context, path string, body []byte, out any) (status int, errCode string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "crispy-companion/1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode >= 400 {
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && ae.Error.Code != "" {
			return resp.StatusCode, ae.Error.Code, nil
		}
		return resp.StatusCode, "", nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return resp.StatusCode, "", fmt.Errorf("decode response: %w", err)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return resp.StatusCode, "", fmt.Errorf("decode data: %w", err)
	}
	return resp.StatusCode, "", nil
}
