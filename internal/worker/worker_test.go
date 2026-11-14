package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/shakedown/internal/muster"
	"github.com/ClearedStack/shakedown/internal/spool"
)

func loadToolAsMoldloftResult(t *testing.T, dir string, extra map[string][]byte) json.RawMessage {
	t.Helper()
	files := map[string]any{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		if slash == "LICENSE" {
			// Exercise base64 object decoding alongside plain UTF-8 strings.
			files[slash] = map[string]string{"base64": base64.StdEncoding.EncodeToString(raw)}
		} else {
			files[slash] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		files[k] = string(v)
	}
	out, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newTestHarness(t *testing.T) (*muster.StandIn, *httptest.Server, *Worker, *spool.Spool, *bytes.Buffer) {
	t.Helper()
	standIn := muster.NewStandIn()
	srv := httptest.NewServer(standIn)
	t.Cleanup(srv.Close)

	spDir := t.TempDir()
	sp, err := spool.Open(spDir)
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	cli := muster.NewClient(srv.URL, muster.StaticTokenSource(standIn.TokenFor("shakedown")))
	w, err := New(Config{
		Dispatcher:        cli,
		Spool:             sp,
		PollInterval:      10 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond,
		CheckTimeout:      5 * time.Second,
		CaseTimeout:       100 * time.Millisecond,
		Log:               &logBuf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return standIn, srv, w, sp, &logBuf
}

func TestWorkerPassProducesChartroomAttestation(t *testing.T) {
	standIn, srv, w, _, logBuf := newTestHarness(t)
	bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/passing-tool", nil)

	if err := standIn.Seed([]muster.SeedItem{{
		ID:    "PA-0001",
		State: "building",
		Lane:  "normal",
		Next:  "shakedown",
		Results: map[string]json.RawMessage{
			"sextant":  json.RawMessage(`{"job":"synthetic summary"}`),
			"moldloft": bundleJSON,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step() = (%v, %v), want (true, nil)", worked, err)
	}

	it, ok, held := standIn.Item("PA-0001")
	if !ok || held {
		t.Fatalf("item ok=%v held=%v", ok, held)
	}
	if it.State != "building" {
		t.Fatalf("state = %s, want building", it.State)
	}
	if got := standIn.NextWorker("PA-0001"); got != "chartroom" {
		t.Fatalf("next worker = %q, want chartroom", got)
	}

	// Verify Chartroom's strict attestation contract on results.shakedown:
	// - top-level keys do not collide with slug, entry, publish, link, merge, retire, provenance
	// - attestation has no unknown fields and has 5 distinct passed=true conditions
	rawRes := it.Results["shakedown"]
	if len(rawRes) == 0 {
		t.Fatal("missing results.shakedown")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawRes, &top); err != nil {
		t.Fatal(err)
	}
	for _, reserved := range []string{"slug", "entry", "publish", "link", "merge", "retire", "provenance"} {
		if _, exists := top[reserved]; exists {
			t.Errorf("results.shakedown collides with Chartroom key %q", reserved)
		}
	}

	var strictAtt struct {
		Conditions []struct {
			Name   string `json:"name"`
			Passed bool   `json:"passed"`
		} `json:"conditions"`
	}
	dec := json.NewDecoder(bytes.NewReader(top["attestation"]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&strictAtt); err != nil {
		t.Fatalf("Chartroom strict attestation decode failed: %v", err)
	}
	if len(strictAtt.Conditions) != 5 {
		t.Fatalf("got %d attested conditions, want 5", len(strictAtt.Conditions))
	}
	seen := map[string]bool{}
	for _, c := range strictAtt.Conditions {
		if !c.Passed || c.Name == "" || seen[c.Name] {
			t.Errorf("invalid attested condition: %+v", c)
		}
		seen[c.Name] = true
	}

	if strings.Contains(logBuf.String(), srv.URL) || strings.Contains(logBuf.String(), "test-token") {
		t.Errorf("logs leaked address or token: %s", logBuf.String())
	}
}

func TestWorkerFailHoldsItemAndOpensBellbuoyIncident(t *testing.T) {
	standIn, _, w, _, _ := newTestHarness(t)
	bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/failing-tool", nil)

	if err := standIn.Seed([]muster.SeedItem{{
		ID:      "PA-0002",
		State:   "building",
		Lane:    "normal",
		Next:    "shakedown",
		Results: map[string]json.RawMessage{"moldloft": bundleJSON},
	}}); err != nil {
		t.Fatal(err)
	}

	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step() = (%v, %v)", worked, err)
	}

	it, _, held := standIn.Item("PA-0002")
	if !held || it.State != "building" {
		t.Fatalf("expected held=true state=building, got held=%v state=%s", held, it.State)
	}
	if standIn.NextWorker("PA-0002") != "" {
		t.Fatalf("expected held item not to advance to chartroom")
	}
}

func TestWorkerParksRegulatoryAmbiguity(t *testing.T) {
	standIn, _, w, _, _ := newTestHarness(t)
	if err := standIn.Seed([]muster.SeedItem{{
		ID:    "PA-0003",
		State: "building",
		Lane:  "normal",
		Next:  "shakedown",
		Results: map[string]json.RawMessage{
			"moldloft": json.RawMessage(`{"status":"regulatory_ambiguity","regulatory_ambiguity":true}`),
		},
	}}); err != nil {
		t.Fatal(err)
	}

	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step() = (%v, %v)", worked, err)
	}

	it, _, held := standIn.Item("PA-0003")
	if held || it.State != "parked" {
		t.Fatalf("expected state=parked held=false, got state=%s held=%v", it.State, held)
	}
	if standIn.NextWorker("PA-0003") != "" {
		t.Fatalf("parked item must leave queue")
	}
}

func TestWorkerTimeoutRequeuesForRetry(t *testing.T) {
	standIn, _, w, _, _ := newTestHarness(t)
	w.caseTimeout = 25 * time.Millisecond

	bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/passing-tool", map[string][]byte{
		"calc.js": []byte(`function calculate(x) { while (true) {} }`),
	})
	if err := standIn.Seed([]muster.SeedItem{{
		ID:      "PA-0004",
		State:   "building",
		Lane:    "normal",
		Next:    "shakedown",
		Results: map[string]json.RawMessage{"moldloft": bundleJSON},
	}}); err != nil {
		t.Fatal(err)
	}

	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step() = (%v, %v)", worked, err)
	}

	it, _, held := standIn.Item("PA-0004")
	if held || it.State != "building" {
		t.Fatalf("expected E_TIMEOUT to keep item unheld in building, got held=%v state=%s", held, it.State)
	}
	if got := standIn.NextWorker("PA-0004"); got != "shakedown" {
		t.Fatalf("next worker = %q, want shakedown for retry", got)
	}
}

func TestWorkerNeedsSubtoolBothContractModes(t *testing.T) {
	t.Run("current contract 409 move_not_allowed drops cleanly without publishing", func(t *testing.T) {
		standIn, _, w, sp, _ := newTestHarness(t)
		bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/passing-tool", map[string][]byte{
			"diagram.svg": []byte(`<svg></svg>`),
		})
		if err := standIn.Seed([]muster.SeedItem{{
			ID:      "PA-0005",
			State:   "building",
			Lane:    "normal",
			Next:    "shakedown",
			Results: map[string]json.RawMessage{"moldloft": bundleJSON},
		}}); err != nil {
			t.Fatal(err)
		}

		worked, err := w.Step(context.Background())
		if err != nil || !worked {
			t.Fatalf("Step() = (%v, %v)", worked, err)
		}
		if n, _ := sp.Pending(); n != 0 {
			t.Fatalf("expected spool to drop permanent 409, got %d pending", n)
		}
		if got := standIn.NextWorker("PA-0005"); got == "chartroom" {
			t.Fatal("unfamiliar bundle must never advance to chartroom")
		}
	})

	t.Run("when muster allows building to needs_subtool item pauses cleanly", func(t *testing.T) {
		standIn, _, w, _, _ := newTestHarness(t)
		standIn.SetAllowPauseBuilding(true)

		bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/passing-tool", map[string][]byte{
			"diagram.svg": []byte(`<svg></svg>`),
		})
		if err := standIn.Seed([]muster.SeedItem{{
			ID:      "PA-0006",
			State:   "building",
			Lane:    "normal",
			Next:    "shakedown",
			Results: map[string]json.RawMessage{"moldloft": bundleJSON},
		}}); err != nil {
			t.Fatal(err)
		}

		worked, err := w.Step(context.Background())
		if err != nil || !worked {
			t.Fatalf("Step() = (%v, %v)", worked, err)
		}
		it, _, _ := standIn.Item("PA-0006")
		if it.State != "needs_subtool" {
			t.Fatalf("state = %s, want needs_subtool", it.State)
		}
	})
}

func TestWorkerCleanRoomRejectsRawSextantCapture(t *testing.T) {
	standIn, _, w, _, _ := newTestHarness(t)
	bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/passing-tool", nil)

	if err := standIn.Seed([]muster.SeedItem{{
		ID:    "PA-0007",
		State: "building",
		Lane:  "normal",
		Next:  "shakedown",
		Results: map[string]json.RawMessage{
			"sextant":  json.RawMessage(`{"raw_html":"<html>captured</html>"}`),
			"moldloft": bundleJSON,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("Step() = (%v, %v)", worked, err)
	}
	_, _, held := standIn.Item("PA-0007")
	if !held {
		t.Fatal("expected item with raw Sextant HTML capture to be held with E_PARSE")
	}
}

func TestWorkerSpoolSurvivesStoreSlowOutage(t *testing.T) {
	standIn, srv, _, sp, _ := newTestHarness(t)
	bundleJSON := loadToolAsMoldloftResult(t, "../../testdata/tools/passing-tool", nil)
	if err := standIn.Seed([]muster.SeedItem{{
		ID:      "PA-0008",
		State:   "building",
		Lane:    "normal",
		Next:    "shakedown",
		Results: map[string]json.RawMessage{"moldloft": bundleJSON},
	}}); err != nil {
		t.Fatal(err)
	}

	// Wrap dispatcher so Complete fails once with 503 store_slow after Lease succeeds.
	realCli := muster.NewClient(srv.URL, muster.StaticTokenSource(standIn.TokenFor("shakedown")))
	flaky := &flakyCompleteDispatcher{Dispatcher: realCli, failNextComplete: true}
	w, err := New(Config{
		Dispatcher: flaky,
		Spool:      sp,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.Step(context.Background())
	if err == nil || !muster.IsRetryable(err) {
		t.Fatalf("expected retryable 503 error on first Step, got %v", err)
	}
	if n, _ := sp.Pending(); n != 1 {
		t.Fatalf("pending = %d, want 1 staged entry", n)
	}

	// Next Step drains the staged Complete before leasing anything new.
	worked, err := w.Step(context.Background())
	if err != nil || worked {
		t.Fatalf("recovery Step() = (%v, %v), want (false, nil)", worked, err)
	}
	if n, _ := sp.Pending(); n != 0 {
		t.Fatalf("pending = %d, want 0 after recovery", n)
	}
	if got := standIn.NextWorker("PA-0008"); got != "chartroom" {
		t.Fatalf("next worker = %q, want chartroom after spool replay", got)
	}
}

type flakyCompleteDispatcher struct {
	muster.Dispatcher
	failNextComplete bool
}

func (f *flakyCompleteDispatcher) Complete(ctx context.Context, lease, state string, result json.RawMessage) (*muster.Ack, error) {
	if f.failNextComplete {
		f.failNextComplete = false
		return nil, &muster.Refusal{
			Status: 503,
			Code:   muster.CodeTimeout,
			Cause:  "store_slow",
			Reason: "the record store took too long",
		}
	}
	return f.Dispatcher.Complete(ctx, lease, state, result)
}

func loadMoldloftGoldenResult(t *testing.T, dir, template, slug string, arrayForm bool) json.RawMessage {
	t.Helper()
	var arrayEntries []map[string]string
	mapEntries := map[string]any{}

	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		if arrayForm {
			if slash == "LICENSE" {
				arrayEntries = append(arrayEntries, map[string]string{
					"path":   slash,
					"base64": base64.StdEncoding.EncodeToString(raw),
				})
			} else {
				arrayEntries = append(arrayEntries, map[string]string{
					"path":    slash,
					"content": string(raw),
				})
			}
		} else {
			if slash == "LICENSE" {
				mapEntries[slash] = map[string]string{"base64": base64.StdEncoding.EncodeToString(raw)}
			} else {
				mapEntries[slash] = string(raw)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"publish": map[string]any{
			"repository": "https://github.com/clearedstack/" + slug,
		},
		"build": map[string]any{
			"contract_version": "v1",
			"template":         template,
			"tool_slug":        slug,
			"committed":        true,
		},
	}
	if arrayForm {
		payload["files"] = arrayEntries
	} else {
		payload["files"] = mapEntries
	}
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkerVerifiesAllFourMoldloftTemplates(t *testing.T) {
	cases := []struct {
		id        string
		fixture   string
		template  string
		slug      string
		arrayForm bool
	}{
		{"PA-1001", "calculator-rate-buildup", "calculator", "sample-rate-buildup", false},
		{"PA-1002", "checklist-solicitation-review", "checklist", "sample-solicitation-review", true},
		{"PA-1003", "decision-tree-clause-routing", "decision-tree", "sample-clause-routing", false},
		{"PA-1004", "validator-line-code-check", "validator", "sample-line-code-check", true},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			standIn, _, w, _, _ := newTestHarness(t)
			bundleJSON := loadMoldloftGoldenResult(t, filepath.Join("../../testdata/moldloft", tc.fixture), tc.template, tc.slug, tc.arrayForm)

			if err := standIn.Seed([]muster.SeedItem{{
				ID:    tc.id,
				State: "building",
				Lane:  "normal",
				Next:  "shakedown",
				Results: map[string]json.RawMessage{
					"waybill":  json.RawMessage(`{"template":"` + tc.template + `"}`),
					"moldloft": bundleJSON,
				},
			}}); err != nil {
				t.Fatal(err)
			}

			worked, err := w.Step(context.Background())
			if err != nil || !worked {
				t.Fatalf("Step() = (%v, %v), want (true, nil)", worked, err)
			}

			it, ok, held := standIn.Item(tc.id)
			if !ok || held || it.State != "building" {
				t.Fatalf("item ok=%v held=%v state=%s", ok, held, it.State)
			}
			if got := standIn.NextWorker(tc.id); got != "chartroom" {
				t.Fatalf("next worker = %q, want chartroom", got)
			}

			var top map[string]json.RawMessage
			if err := json.Unmarshal(it.Results["shakedown"], &top); err != nil {
				t.Fatal(err)
			}
			var strictAtt struct {
				Conditions []struct {
					Name   string `json:"name"`
					Passed bool   `json:"passed"`
				} `json:"conditions"`
			}
			dec := json.NewDecoder(bytes.NewReader(top["attestation"]))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&strictAtt); err != nil {
				t.Fatalf("attestation decode failed: %v", err)
			}
			if len(strictAtt.Conditions) != 5 {
				t.Fatalf("got %d conditions, want 5", len(strictAtt.Conditions))
			}
			for _, c := range strictAtt.Conditions {
				if !c.Passed {
					t.Errorf("condition %s did not pass", c.Name)
				}
			}
		})
	}
}

func TestDecodeMoldloftBundleRejectsMalformedEnvelopes(t *testing.T) {
	badInputs := []string{
		`{"publish":{"repository":"https://github.com/clearedstack/demo"},"build":{"contract_version":"v1"}}`,
		`{"publish":"not-an-object","files":{"index.html":"x"}}`,
		`{"files":[{"path":"a.txt","content":"1"},{"path":"a.txt","content":"2"}]}`,
		`{"files":[{"path":"a.txt","content":"1","base64":"MQ=="}]}`,
		`{"files":[{"path":"a.txt"}]}`,
	}
	for i, raw := range badInputs {
		if _, amb, err := decodeMoldloftBundle(json.RawMessage(raw)); err == nil || amb {
			t.Errorf("case %d (%s): expected error, got amb=%v err=%v", i, raw, amb, err)
		}
	}
}
