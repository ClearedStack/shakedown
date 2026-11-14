// Package muster speaks ClearedStack worker contract v1 over HTTP.
//
// Outbound requests carry only fields defined by contract/v1/CONTRACT.md, a
// bearer token from the mounted ServiceAccount secret, and bodies capped at
// 64 KiB. Errors returned by Client contain only the three public error codes
// and fixed contract text, never URLs, hostnames, IP addresses, paths, or
// credentials.
package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Version is the only shared contract version this build speaks.
const Version = "v1"

// Public error codes permitted on the wire and in logs.
const (
	CodeTimeout      = "E_TIMEOUT"
	CodeParse        = "E_PARSE"
	CodeNeedsSubtool = "E_NEEDS_SUBTOOL"
)

// MaxBody is Muster's 64 KiB limit on worker request bodies.
const MaxBody = 64 << 10

// MaxResponseBody bounds how much of a Muster response Client will read.
const MaxResponseBody = 1 << 20

// Dispatcher is the narrow worker interface Shakedown uses to talk to Muster.
type Dispatcher interface {
	Lease(ctx context.Context) (*Grant, error)
	Heartbeat(ctx context.Context, lease string) (*Beat, error)
	Complete(ctx context.Context, lease, state string, result json.RawMessage) (*Ack, error)
	Fail(ctx context.Context, lease, code string) (*Ack, error)
	NeedsSubtool(ctx context.Context, lease string) (*Ack, error)
}

// Grant is the 200 answer to POST /v1/lease.
type Grant struct {
	Lease    string                     `json:"lease"`
	Kind     string                     `json:"kind"`
	ID       string                     `json:"id"`
	State    string                     `json:"state"`
	Lane     string                     `json:"lane"`
	Attempt  int                        `json:"attempt"`
	Expires  string                     `json:"expires"`
	Found    *FoundView                 `json:"found,omitempty"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Proposal *ProposalView              `json:"proposal,omitempty"`
	Incident *IncidentView              `json:"incident,omitempty"`
}

// FoundView describes where an item was discovered.
type FoundView struct {
	Source  string `json:"source"`
	Title   string `json:"title"`
	Address string `json:"address"`
}

// ProposalView describes an anonymous edit proposal.
type ProposalView struct {
	Target     string `json:"target"`
	Repository string `json:"repository"`
	File       string `json:"file"`
	EditKind   string `json:"edit_kind"`
	Change     string `json:"change"`
	Reason     string `json:"reason"`
}

// IncidentView describes a stopped or paused item step assigned to Bellbuoy.
type IncidentView struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Code   string `json:"code"`
	State  string `json:"state"`
}

// Beat is the 200 answer to POST /v1/heartbeat.
type Beat struct {
	Lease   string `json:"lease"`
	Expires string `json:"expires"`
}

// Ack is the 200 answer to complete, fail, needs_subtool, and release.
type Ack struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Next  string `json:"next,omitempty"`
	Held  bool   `json:"held,omitempty"`
}

// Refusal is a structured non-2xx answer from Muster.
type Refusal struct {
	Status int    `json:"-"`
	Code   string `json:"error"`
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s (%d %s): %s", r.Code, r.Status, r.Cause, r.Reason)
}

// Retryable reports whether the caller should keep its spooled state and try
// again later.
func (r *Refusal) Retryable() bool {
	return r.Code == CodeTimeout || r.Status == http.StatusServiceUnavailable || r.Status == http.StatusTooManyRequests
}

// ClientError represents a local validation, token, or transport error with a
// sanitized message and one of the three public error codes.
type ClientError struct {
	Code      string
	Message   string
	Transient bool
}

func (e *ClientError) Error() string {
	return e.Code + ": " + e.Message
}

// IsRetryable reports whether err is a transient transport or Muster error
// that warrants retrying a spooled call.
func IsRetryable(err error) bool {
	var ref *Refusal
	if errors.As(err, &ref) {
		return ref.Retryable()
	}
	var ce *ClientError
	if errors.As(err, &ce) {
		return ce.Transient
	}
	return false
}

// IsPermanentRefusal reports whether Muster definitively refused the call with
// a non-retryable 4xx status (for example 409 lease_not_held or move_not_allowed).
func IsPermanentRefusal(err error) bool {
	var ref *Refusal
	if errors.As(err, &ref) {
		return !ref.Retryable() && ref.Status >= 400 && ref.Status < 500
	}
	return false
}

// TokenSource returns the current bearer token for outbound calls.
type TokenSource func() (string, error)

// FileTokenSource reads and trims a mounted ServiceAccount token file on each
// call so rotated tokens are picked up without restarting the worker.
func FileTokenSource(path string) TokenSource {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", &ClientError{Code: CodeParse, Message: "service account token could not be read"}
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			return "", &ClientError{Code: CodeParse, Message: "service account token is empty"}
		}
		return tok, nil
	}
}

// StaticTokenSource returns a fixed bearer token (intended for tests).
func StaticTokenSource(token string) TokenSource {
	return func() (string, error) {
		if token == "" {
			return "", &ClientError{Code: CodeParse, Message: "service account token is empty"}
		}
		return token, nil
	}
}

// Client talks to Muster over HTTP.
type Client struct {
	BaseURL    string
	Token      TokenSource
	HTTPClient *http.Client
}

// NewClient constructs a Muster HTTP client with bounded timeouts.
func NewClient(baseURL string, token TokenSource) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type leaseWire struct {
	Contract string `json:"contract"`
}

type leaseIDWire struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

type completeWire struct {
	Contract string          `json:"contract"`
	Lease    string          `json:"lease"`
	State    string          `json:"state"`
	Result   json.RawMessage `json:"result,omitempty"`
}

type failWire struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
	Code     string `json:"code"`
}

type releaseWire struct {
	Contract string `json:"contract"`
	ID       string `json:"id"`
}

// Lease asks Muster for the next step queued for this worker. It returns
// (nil, nil) when Muster responds 204 No Content.
func (c *Client) Lease(ctx context.Context) (*Grant, error) {
	var grant Grant
	status, err := c.post(ctx, "/v1/lease", leaseWire{Contract: Version}, &grant)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &grant, nil
}

// Heartbeat extends an active lease held by this worker.
func (c *Client) Heartbeat(ctx context.Context, lease string) (*Beat, error) {
	var beat Beat
	if _, err := c.post(ctx, "/v1/heartbeat", leaseIDWire{Contract: Version, Lease: lease}, &beat); err != nil {
		return nil, err
	}
	return &beat, nil
}

// Complete reports a finished step, its target state, and optional JSON object result.
func (c *Client) Complete(ctx context.Context, lease, state string, result json.RawMessage) (*Ack, error) {
	var clean json.RawMessage
	if len(bytes.TrimSpace(result)) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, result); err != nil {
			return nil, &ClientError{Code: CodeParse, Message: "result is not valid json"}
		}
		b := buf.Bytes()
		if len(b) == 0 || b[0] != '{' {
			return nil, &ClientError{Code: CodeParse, Message: "result must be a json object"}
		}
		clean = b
	}
	var ack Ack
	if _, err := c.post(ctx, "/v1/complete", completeWire{
		Contract: Version,
		Lease:    lease,
		State:    state,
		Result:   clean,
	}, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// Fail reports E_PARSE or E_TIMEOUT on a held lease.
func (c *Client) Fail(ctx context.Context, lease, code string) (*Ack, error) {
	var ack Ack
	if _, err := c.post(ctx, "/v1/fail", failWire{
		Contract: Version,
		Lease:    lease,
		Code:     code,
	}, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// NeedsSubtool pauses an item when the worker encounters valid, unfamiliar input.
func (c *Client) NeedsSubtool(ctx context.Context, lease string) (*Ack, error) {
	var ack Ack
	if _, err := c.post(ctx, "/v1/needs_subtool", leaseIDWire{
		Contract: Version,
		Lease:    lease,
	}, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// Release sends POST /v1/release (defined in contract v1 for Countersign).
func (c *Client) Release(ctx context.Context, id string) (*Ack, error) {
	var ack Ack
	if _, err := c.post(ctx, "/v1/release", releaseWire{
		Contract: Version,
		ID:       id,
	}, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func (c *Client) post(ctx context.Context, path string, payload any, dst any) (int, error) {
	if c.BaseURL == "" {
		return 0, &ClientError{Code: CodeParse, Message: "dispatcher url is not configured"}
	}
	if c.Token == nil {
		return 0, &ClientError{Code: CodeParse, Message: "service account token source is not configured"}
	}
	tok, err := c.Token()
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, &ClientError{Code: CodeParse, Message: "request body could not be encoded"}
	}
	if len(body) > MaxBody {
		return 0, &ClientError{Code: CodeParse, Message: "request body exceeds 64 KiB contract limit"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, &ClientError{Code: CodeParse, Message: "request could not be constructed"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Never propagate net/url.Error, which embeds the target URL and host.
		return 0, &ClientError{
			Code:      CodeTimeout,
			Message:   "dispatcher call did not complete in time",
			Transient: true,
		}
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody+1))
	if err != nil {
		return 0, &ClientError{
			Code:      CodeTimeout,
			Message:   "dispatcher response could not be read",
			Transient: true,
		}
	}
	if len(respBytes) > MaxResponseBody {
		return 0, &ClientError{Code: CodeParse, Message: "dispatcher response exceeded limit"}
	}

	if resp.StatusCode == http.StatusNoContent {
		if len(bytes.TrimSpace(respBytes)) != 0 {
			return 0, &ClientError{Code: CodeParse, Message: "unexpected body on 204 response"}
		}
		return http.StatusNoContent, nil
	}

	if resp.StatusCode >= 400 {
		var ref Refusal
		if err := decodeStrict(respBytes, &ref); err != nil || ref.Code == "" || ref.Cause == "" || ref.Reason == "" {
			transient := resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			code := CodeParse
			if transient {
				code = CodeTimeout
			}
			return 0, &ClientError{
				Code:      code,
				Message:   "dispatcher returned an unrecognized refusal",
				Transient: transient,
			}
		}
		ref.Status = resp.StatusCode
		return resp.StatusCode, &ref
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, &ClientError{Code: CodeParse, Message: "dispatcher returned an unexpected status"}
	}

	if dst != nil {
		if err := decodeStrict(respBytes, dst); err != nil {
			return 0, &ClientError{Code: CodeParse, Message: "dispatcher response did not match contract"}
		}
	}
	return resp.StatusCode, nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}
