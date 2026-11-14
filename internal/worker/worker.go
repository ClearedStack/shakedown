// Package worker runs Shakedown's Muster lease, heartbeat, check, spool, and
// report loop.
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ClearedStack/shakedown/internal/check"
	"github.com/ClearedStack/shakedown/internal/muster"
	"github.com/ClearedStack/shakedown/internal/spool"
)

// Default intervals and deadlines for worker mode.
const (
	DefaultPollInterval      = 2 * time.Second
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultCheckTimeout      = 30 * time.Second
	DefaultCaseTimeout       = 2 * time.Second
	MaxBackoff               = 30 * time.Second
)

// AttestedCondition is one entry in the attestation block Chartroom requires
// inside results.shakedown before publishing an item to live.
type AttestedCondition struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// Attestation is the closed object Chartroom reads from results.shakedown.
type Attestation struct {
	Conditions []AttestedCondition `json:"conditions"`
}

// PassResult is the JSON object Shakedown sends in POST /v1/complete when all
// five listing conditions pass.
type PassResult struct {
	Component   string                  `json:"component"`
	Version     string                  `json:"version"`
	Contract    string                  `json:"contract"`
	Verdict     check.Status            `json:"verdict"`
	Conditions  []check.ConditionResult `json:"conditions"`
	Attestation Attestation             `json:"attestation"`
}

// ParkedResult is the JSON object Shakedown sends in POST /v1/complete when
// regulatory ambiguity blocks automated verification.
type ParkedResult struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Contract  string `json:"contract"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
}

// Config configures a Worker instance.
type Config struct {
	Dispatcher        muster.Dispatcher
	Spool             *spool.Spool
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	CheckTimeout      time.Duration
	CaseTimeout       time.Duration
	Log               io.Writer
}

// Worker leases building items from Muster, verifies static bundles in memory,
// stages results in /var/spool/cs, and reports outcomes back to Muster.
type Worker struct {
	disp              muster.Dispatcher
	spool             *spool.Spool
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	checkTimeout      time.Duration
	caseTimeout       time.Duration
	logMu             sync.Mutex
	log               io.Writer
}

// New constructs a Worker with bounded defaults for any unset durations.
func New(cfg Config) (*Worker, error) {
	if cfg.Dispatcher == nil {
		return nil, errors.New("E_PARSE: dispatcher is required")
	}
	if cfg.Spool == nil {
		return nil, errors.New("E_PARSE: outbound spool is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = DefaultCheckTimeout
	}
	if cfg.CaseTimeout <= 0 {
		cfg.CaseTimeout = DefaultCaseTimeout
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	return &Worker{
		disp:              cfg.Dispatcher,
		spool:             cfg.Spool,
		pollInterval:      cfg.PollInterval,
		heartbeatInterval: cfg.HeartbeatInterval,
		checkTimeout:      cfg.CheckTimeout,
		caseTimeout:       cfg.CaseTimeout,
		log:               cfg.Log,
	}, nil
}

func (w *Worker) logf(format string, args ...any) {
	w.logMu.Lock()
	defer w.logMu.Unlock()
	_, _ = fmt.Fprintf(w.log, "shakedown: "+format+"\n", args...)
}

// Run polls Muster until ctx is cancelled, draining spooled reports before each
// new lease and backing off deterministically on transient dispatcher errors.
func (w *Worker) Run(ctx context.Context) error {
	backoff := w.pollInterval
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		worked, err := w.Step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logf("%s", sanitizeErr(err))
			if !sleepContext(ctx, backoff) {
				return nil
			}
			backoff *= 2
			if backoff > MaxBackoff {
				backoff = MaxBackoff
			}
			continue
		}
		backoff = w.pollInterval
		if worked {
			continue
		}
		if !sleepContext(ctx, w.pollInterval) {
			return nil
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func sanitizeErr(err error) string {
	var ref *muster.Refusal
	if errors.As(err, &ref) {
		return ref.Error()
	}
	var ce *muster.ClientError
	if errors.As(err, &ce) {
		return ce.Error()
	}
	msg := err.Error()
	for _, code := range []string{muster.CodeParse, muster.CodeTimeout, muster.CodeNeedsSubtool} {
		if strings.HasPrefix(msg, code) {
			return msg
		}
	}
	return muster.CodeParse + ": worker step error"
}

// Step drains any spooled reports and then processes at most one leased item.
// It returns worked=true when a lease was received and handled.
func (w *Worker) Step(ctx context.Context) (bool, error) {
	drained, err := w.spool.Drain(ctx, w.disp)
	for _, warn := range drained.Warnings {
		w.logf("spool notice: %s", warn)
	}
	if err != nil {
		return false, err
	}

	grant, err := w.disp.Lease(ctx)
	if err != nil {
		return false, err
	}
	if grant == nil {
		return false, nil
	}

	if grant.Kind != "item" || grant.State != "building" {
		w.logf("%s: unexpected grant kind=%s state=%s", muster.CodeParse, safeToken(grant.Kind), safeToken(grant.State))
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:  spool.KindFail,
			Lease: grant.Lease,
			Code:  muster.CodeParse,
		})
	}

	if hasRawSextantCapture(grant.Results["sextant"]) {
		w.logf("%s: item %s rejected due to raw observation capture in upstream results", muster.CodeParse, safeToken(grant.ID))
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:  spool.KindFail,
			Lease: grant.Lease,
			Code:  muster.CodeParse,
		})
	}

	bundleFiles, ambiguous, parseErr := decodeMoldloftBundle(grant.Results["moldloft"])
	if parseErr != nil {
		w.logf("%s: item %s malformed moldloft bundle", muster.CodeParse, safeToken(grant.ID))
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:  spool.KindFail,
			Lease: grant.Lease,
			Code:  muster.CodeParse,
		})
	}

	if ambiguous {
		resBytes, _ := json.Marshal(ParkedResult{
			Component: check.Component,
			Version:   check.Version,
			Contract:  check.Contract,
			Verdict:   "parked",
			Reason:    "regulatory_ambiguity",
		})
		w.logf("item %s parked due to regulatory ambiguity", safeToken(grant.ID))
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:   spool.KindComplete,
			Lease:  grant.Lease,
			State:  "parked",
			Result: resBytes,
		})
	}

	report, lostLease := w.runCheckWithHeartbeats(ctx, grant.Lease, bundleFiles)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if lostLease {
		w.logf("%s: item %s lease lost during verification", muster.CodeParse, safeToken(grant.ID))
		return true, nil
	}

	switch report.Verdict {
	case check.Pass:
		attested := make([]AttestedCondition, 0, len(report.Conditions))
		for _, c := range report.Conditions {
			attested = append(attested, AttestedCondition{
				Name:   c.Condition,
				Passed: c.Status == check.Pass,
			})
		}
		resBytes, err := json.Marshal(PassResult{
			Component:   report.Component,
			Version:     report.Version,
			Contract:    report.Contract,
			Verdict:     report.Verdict,
			Conditions:  report.Conditions,
			Attestation: Attestation{Conditions: attested},
		})
		if err != nil {
			return true, w.stageAndDrain(ctx, spool.Entry{
				Kind:  spool.KindFail,
				Lease: grant.Lease,
				Code:  muster.CodeParse,
			})
		}
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:   spool.KindComplete,
			Lease:  grant.Lease,
			State:  "building",
			Result: resBytes,
		})

	case check.NeedsSubtool:
		w.logFindings(grant.ID, report)
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:  spool.KindNeedsSubtool,
			Lease: grant.Lease,
		})

	case check.Timeout:
		w.logFindings(grant.ID, report)
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:  spool.KindFail,
			Lease: grant.Lease,
			Code:  muster.CodeTimeout,
		})

	default:
		w.logFindings(grant.ID, report)
		return true, w.stageAndDrain(ctx, spool.Entry{
			Kind:  spool.KindFail,
			Lease: grant.Lease,
			Code:  muster.CodeParse,
		})
	}
}

func (w *Worker) logFindings(itemID string, report check.Report) {
	rules := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		rules = append(rules, string(f.Code)+":"+f.Rule)
	}
	w.logf("item %s verdict=%s findings=[%s]", safeToken(itemID), report.Verdict, strings.Join(rules, " "))
}

func (w *Worker) stageAndDrain(ctx context.Context, e spool.Entry) error {
	if _, err := w.spool.Stage(e); err != nil {
		return err
	}
	drained, err := w.spool.Drain(ctx, w.disp)
	for _, warn := range drained.Warnings {
		w.logf("spool notice: %s", warn)
	}
	return err
}

func (w *Worker) runCheckWithHeartbeats(ctx context.Context, lease string, files map[string][]byte) (check.Report, bool) {
	checkCtx, cancelCheck := context.WithTimeout(ctx, w.checkTimeout)
	defer cancelCheck()

	hbStop := make(chan struct{})
	hbDone := make(chan bool, 1)

	go func() {
		ticker := time.NewTicker(w.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbStop:
				hbDone <- false
				return
			case <-ctx.Done():
				hbDone <- false
				return
			case <-ticker.C:
				_, err := w.disp.Heartbeat(ctx, lease)
				if err != nil {
					var ref *muster.Refusal
					if errors.As(err, &ref) && ref.Cause == "lease_not_held" {
						cancelCheck()
						hbDone <- true
						return
					}
					w.logf("heartbeat warning: %s", sanitizeErr(err))
				}
			}
		}
	}()

	report := check.Map(checkCtx, files, check.Options{CaseTimeout: w.caseTimeout})
	close(hbStop)
	lostLease := <-hbDone
	return report, lostLease
}

var rawObservationKeys = map[string]bool{
	"raw_html":     true,
	"page_html":    true,
	"dom":          true,
	"dom_snapshot": true,
	"screenshot":   true,
	"snapshot":     true,
	"har":          true,
	"archive":      true,
	"body_bytes":   true,
	"cookies":      true,
}

func hasRawSextantCapture(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return true
	}
	for k := range obj {
		if rawObservationKeys[strings.ToLower(k)] {
			return true
		}
	}
	return false
}

type moldloftWire struct {
	Files               json.RawMessage `json:"files,omitempty"`
	Publish             json.RawMessage `json:"publish,omitempty"`
	Build               json.RawMessage `json:"build,omitempty"`
	Status              string          `json:"status,omitempty"`
	RegulatoryAmbiguity bool            `json:"regulatory_ambiguity,omitempty"`
}

type base64FileWire struct {
	Base64 string `json:"base64"`
}

type arrayFileWire struct {
	Path    string  `json:"path"`
	Content *string `json:"content,omitempty"`
	Base64  *string `json:"base64,omitempty"`
}

func requireJSONObjectIfPresent(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '{' {
		return errors.New("metadata block must be a JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return errors.New("invalid metadata object")
	}
	return nil
}

func decodeMoldloftBundle(raw json.RawMessage) (map[string][]byte, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, errors.New("missing moldloft result")
	}
	var payload moldloftWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, false, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false, errors.New("trailing data")
	}
	if err := requireJSONObjectIfPresent(payload.Publish); err != nil {
		return nil, false, err
	}
	if err := requireJSONObjectIfPresent(payload.Build); err != nil {
		return nil, false, err
	}

	if payload.RegulatoryAmbiguity || payload.Status == "regulatory_ambiguity" {
		return nil, true, nil
	}
	if payload.Status != "" && payload.Status != "ready" && payload.Status != "built" {
		return nil, false, errors.New("unrecognized moldloft status")
	}

	filesRaw := bytes.TrimSpace(payload.Files)
	if len(filesRaw) == 0 {
		return nil, false, errors.New("empty moldloft files")
	}

	switch filesRaw[0] {
	case '{':
		var fileMap map[string]json.RawMessage
		if err := json.Unmarshal(filesRaw, &fileMap); err != nil || len(fileMap) == 0 {
			return nil, false, errors.New("empty moldloft files")
		}
		out := make(map[string][]byte, len(fileMap))
		for name, entryRaw := range fileMap {
			trimmed := bytes.TrimSpace(entryRaw)
			if len(trimmed) == 0 {
				return nil, false, errors.New("empty file value")
			}
			switch trimmed[0] {
			case '"':
				var text string
				if err := json.Unmarshal(trimmed, &text); err != nil {
					return nil, false, err
				}
				out[name] = []byte(text)
			case '{':
				var b64 base64FileWire
				fdec := json.NewDecoder(bytes.NewReader(trimmed))
				fdec.DisallowUnknownFields()
				if err := fdec.Decode(&b64); err != nil {
					return nil, false, err
				}
				if _, err := fdec.Token(); err != io.EOF {
					return nil, false, errors.New("trailing data")
				}
				decoded, err := base64.StdEncoding.DecodeString(b64.Base64)
				if err != nil {
					return nil, false, err
				}
				out[name] = decoded
			default:
				return nil, false, errors.New("invalid file encoding")
			}
		}
		return out, false, nil

	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(filesRaw, &list); err != nil || len(list) == 0 {
			return nil, false, errors.New("empty moldloft files")
		}
		out := make(map[string][]byte, len(list))
		for _, elemRaw := range list {
			var entry arrayFileWire
			fdec := json.NewDecoder(bytes.NewReader(elemRaw))
			fdec.DisallowUnknownFields()
			if err := fdec.Decode(&entry); err != nil {
				return nil, false, err
			}
			if _, err := fdec.Token(); err != io.EOF {
				return nil, false, errors.New("trailing data")
			}
			if entry.Path == "" {
				return nil, false, errors.New("empty file path")
			}
			if _, dup := out[entry.Path]; dup {
				return nil, false, errors.New("duplicate file path")
			}
			if (entry.Content != nil) == (entry.Base64 != nil) {
				return nil, false, errors.New("file entry must carry either content or base64")
			}
			if entry.Content != nil {
				out[entry.Path] = []byte(*entry.Content)
			} else {
				decoded, err := base64.StdEncoding.DecodeString(*entry.Base64)
				if err != nil {
					return nil, false, err
				}
				out[entry.Path] = decoded
			}
		}
		return out, false, nil

	default:
		return nil, false, errors.New("invalid moldloft files container")
	}
}

func safeToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 48; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
