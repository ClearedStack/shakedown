package spool

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/shakedown/internal/muster"
)

type fakeDispatcher struct {
	calls []string
	err   error
}

func (f *fakeDispatcher) Lease(context.Context) (*muster.Grant, error) {
	return nil, nil
}

func (f *fakeDispatcher) Heartbeat(context.Context, string) (*muster.Beat, error) {
	return &muster.Beat{}, nil
}

func (f *fakeDispatcher) Complete(_ context.Context, lease, state string, _ json.RawMessage) (*muster.Ack, error) {
	f.calls = append(f.calls, "complete:"+lease+":"+state)
	if f.err != nil {
		return nil, f.err
	}
	return &muster.Ack{ID: "PA-0001", State: state}, nil
}

func (f *fakeDispatcher) Fail(_ context.Context, lease, code string) (*muster.Ack, error) {
	f.calls = append(f.calls, "fail:"+lease+":"+code)
	if f.err != nil {
		return nil, f.err
	}
	return &muster.Ack{ID: "PA-0001", State: "building"}, nil
}

func (f *fakeDispatcher) NeedsSubtool(_ context.Context, lease string) (*muster.Ack, error) {
	f.calls = append(f.calls, "needs_subtool:"+lease)
	if f.err != nil {
		return nil, f.err
	}
	return &muster.Ack{ID: "PA-0001", State: "needs_subtool"}, nil
}

func TestStageRetryAndDeliverInOrder(t *testing.T) {
	dir := t.TempDir()
	// Leftover .tmp from crash is cleaned on Open.
	if err := os.WriteFile(filepath.Join(dir, "00000001-old.json.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "00000001-old.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("expected stale .tmp file to be removed on Open")
	}

	if _, err := s.Stage(Entry{
		Kind:   KindComplete,
		Lease:  "PA-0001.1.1",
		State:  "building",
		Result: json.RawMessage(`{"verdict":"pass"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(Entry{
		Kind:  KindFail,
		Lease: "PA-0002.2.1",
		Code:  muster.CodeParse,
	}); err != nil {
		t.Fatal(err)
	}

	// Transient 503 keeps both entries on disk.
	disp := &fakeDispatcher{
		err: &muster.Refusal{
			Status: http.StatusServiceUnavailable,
			Code:   muster.CodeTimeout,
			Cause:  "store_slow",
			Reason: "the record store took too long",
		},
	}
	_, err = s.Drain(context.Background(), disp)
	if err == nil || !muster.IsRetryable(err) {
		t.Fatalf("expected retryable error, got %v", err)
	}
	if n, _ := s.Pending(); n != 2 {
		t.Fatalf("pending = %d, want 2 after transient error", n)
	}

	// Recovery delivers both in exact order and empties the spool.
	disp.err = nil
	disp.calls = nil
	out, err := s.Drain(context.Background(), disp)
	if err != nil {
		t.Fatalf("Drain on recovery: %v", err)
	}
	if out.Delivered != 2 || out.Dropped != 0 {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	wantCalls := "complete:PA-0001.1.1:building,fail:PA-0002.2.1:E_PARSE"
	if got := strings.Join(disp.calls, ","); got != wantCalls {
		t.Fatalf("calls = %q, want %q", got, wantCalls)
	}
	if n, _ := s.Pending(); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}

func TestTamperedAndPermanentRefusalDroppedSafely(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.Stage(Entry{
		Kind:  KindNeedsSubtool,
		Lease: "PA-0003.1.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with the lease inside the staged JSON so SHA-256 verification fails.
	p := filepath.Join(dir, name)
	raw, _ := os.ReadFile(p)
	tampered := strings.Replace(string(raw), "PA-0003.1.1", "PA-9999.1.1", 1)
	if err := os.WriteFile(p, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Stage(Entry{
		Kind:  KindNeedsSubtool,
		Lease: "PA-0004.1.1",
	}); err != nil {
		t.Fatal(err)
	}

	disp := &fakeDispatcher{
		err: &muster.Refusal{
			Status: http.StatusConflict,
			Code:   muster.CodeParse,
			Cause:  "move_not_allowed",
			Reason: "that move is not allowed",
		},
	}
	out, err := s.Drain(context.Background(), disp)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if out.Dropped != 2 {
		t.Fatalf("Dropped = %d, want 2", out.Dropped)
	}
	for _, w := range out.Warnings {
		if strings.Contains(w, dir) {
			t.Errorf("warning leaked spool directory path: %s", w)
		}
	}
	if n, _ := s.Pending(); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}
