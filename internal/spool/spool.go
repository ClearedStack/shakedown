// Package spool stages outbound Muster reports atomically in /var/spool/cs so
// transient Muster or Logbook outages never drop a finished verification step.
package spool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/ClearedStack/shakedown/internal/muster"
)

// Kind identifies which terminal worker call a spooled entry represents.
type Kind string

const (
	KindComplete     Kind = "complete"
	KindFail         Kind = "fail"
	KindNeedsSubtool Kind = "needs_subtool"
)

const (
	maxEntries   = 64
	maxEntrySize = 128 << 10
)

// Entry is one outbound Muster report staged on disk.
type Entry struct {
	Contract string          `json:"contract"`
	Kind     Kind            `json:"kind"`
	Lease    string          `json:"lease"`
	State    string          `json:"state,omitempty"`
	Code     string          `json:"code,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Digest   string          `json:"digest"`
}

// Sanitized errors with no filesystem paths.
var (
	ErrSpoolFull   = errors.New("E_TIMEOUT: outbound spool is full")
	ErrSpoolIO     = errors.New("E_TIMEOUT: outbound spool write failed")
	ErrSpoolFormat = errors.New("E_PARSE: invalid outbound spool entry")
)

// Spool manages staged reports in a single writable directory.
type Spool struct {
	dir string
	seq atomic.Uint64
}

// Open prepares dir as an outbound spool, removing any incomplete .tmp files
// left behind by an abrupt crash.
func Open(dir string) (*Spool, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, ErrSpoolFormat
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, ErrSpoolIO
	}
	var highest uint64
	for _, d := range entries {
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		var n uint64
		if _, scanErr := fmt.Sscanf(name, "%08d-", &n); scanErr == nil && n > highest {
			highest = n
		}
	}
	s := &Spool{dir: dir}
	s.seq.Store(highest)
	return s, nil
}

func computeDigest(e Entry) (string, error) {
	var resCompact []byte
	if len(bytes.TrimSpace(e.Result)) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, e.Result); err != nil {
			return "", ErrSpoolFormat
		}
		resCompact = buf.Bytes()
	}
	h := sha256.New()
	for _, field := range []string{e.Contract, string(e.Kind), e.Lease, e.State, e.Code} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	h.Write(resCompact)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func validateEntry(e *Entry) error {
	if e.Contract == "" {
		e.Contract = muster.Version
	}
	if e.Contract != muster.Version || e.Lease == "" {
		return ErrSpoolFormat
	}
	switch e.Kind {
	case KindComplete:
		if e.State == "" || e.Code != "" {
			return ErrSpoolFormat
		}
		if len(bytes.TrimSpace(e.Result)) > 0 {
			var buf bytes.Buffer
			if err := json.Compact(&buf, e.Result); err != nil {
				return ErrSpoolFormat
			}
			if buf.Len() == 0 || buf.Bytes()[0] != '{' {
				return ErrSpoolFormat
			}
			e.Result = buf.Bytes()
		}
	case KindFail:
		if (e.Code != muster.CodeParse && e.Code != muster.CodeTimeout) || e.State != "" || len(e.Result) > 0 {
			return ErrSpoolFormat
		}
	case KindNeedsSubtool:
		if e.State != "" || e.Code != "" || len(e.Result) > 0 {
			return ErrSpoolFormat
		}
	default:
		return ErrSpoolFormat
	}
	return nil
}

func safeLeaseToken(lease string) string {
	var b strings.Builder
	for i := 0; i < len(lease) && b.Len() < 64; i++ {
		c := lease[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "lease"
	}
	return b.String()
}

// Stage validates e, computes its canonical SHA-256 digest, and writes it
// atomically into the spool directory.
func (s *Spool) Stage(e Entry) (string, error) {
	if err := validateEntry(&e); err != nil {
		return "", err
	}
	digest, err := computeDigest(e)
	if err != nil {
		return "", err
	}
	e.Digest = digest

	existing, err := s.files()
	if err != nil {
		return "", ErrSpoolIO
	}
	if len(existing) >= maxEntries {
		return "", ErrSpoolFull
	}

	raw, err := json.Marshal(e)
	if err != nil || len(raw) > maxEntrySize {
		return "", ErrSpoolFormat
	}
	raw = append(raw, '\n')

	n := s.seq.Add(1)
	finalName := fmt.Sprintf("%08d-%s.json", n, safeLeaseToken(e.Lease))
	tmpPath := filepath.Join(s.dir, finalName+".tmp")
	finalPath := filepath.Join(s.dir, finalName)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", ErrSpoolIO
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", ErrSpoolIO
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", ErrSpoolIO
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", ErrSpoolIO
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", ErrSpoolIO
	}
	if dirFile, err := os.Open(s.dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return finalName, nil
}

// Pending returns how many staged entries are currently waiting on disk.
func (s *Spool) Pending() (int, error) {
	list, err := s.files()
	if err != nil {
		return 0, ErrSpoolIO
	}
	return len(list), nil
}

func (s *Spool) files() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range entries {
		if d.IsDir() {
			continue
		}
		if strings.HasSuffix(d.Name(), ".json") {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func readSpooledEntry(path string) (Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return Entry{}, ErrSpoolIO
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxEntrySize+1))
	if err != nil {
		return Entry{}, ErrSpoolIO
	}
	if len(raw) > maxEntrySize {
		return Entry{}, ErrSpoolFormat
	}
	var e Entry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Entry{}, ErrSpoolFormat
	}
	if _, err := dec.Token(); err != io.EOF {
		return Entry{}, ErrSpoolFormat
	}
	if err := validateEntry(&e); err != nil {
		return Entry{}, err
	}
	wantDigest, err := computeDigest(e)
	if err != nil || e.Digest != wantDigest {
		return Entry{}, ErrSpoolFormat
	}
	return e, nil
}

// DrainOutcome summarizes one Drain pass over staged reports.
type DrainOutcome struct {
	Delivered int
	Dropped   int
	Warnings  []string
}

// Drain replays all staged reports in order through disp.
//
// An entry acknowledged with 200 OK is deleted. An entry refused permanently
// with a non-retryable 4xx status (for example 409 lease_not_held or
// 409 move_not_allowed) or a corrupted file on disk is dropped and recorded in
// Warnings without filesystem paths. A retryable error stops Drain and leaves
// the remaining entries staged on disk for the next attempt.
func (s *Spool) Drain(ctx context.Context, disp muster.Dispatcher) (DrainOutcome, error) {
	var out DrainOutcome
	names, err := s.files()
	if err != nil {
		return out, ErrSpoolIO
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		full := filepath.Join(s.dir, name)
		e, err := readSpooledEntry(full)
		if err != nil {
			if errors.Is(err, ErrSpoolFormat) {
				_ = os.Remove(full)
				out.Dropped++
				out.Warnings = append(out.Warnings, muster.CodeParse+": dropped corrupted spool entry")
				continue
			}
			return out, err
		}

		var sendErr error
		switch e.Kind {
		case KindComplete:
			_, sendErr = disp.Complete(ctx, e.Lease, e.State, e.Result)
		case KindFail:
			_, sendErr = disp.Fail(ctx, e.Lease, e.Code)
		case KindNeedsSubtool:
			_, sendErr = disp.NeedsSubtool(ctx, e.Lease)
		}

		if sendErr == nil {
			_ = os.Remove(full)
			out.Delivered++
			continue
		}
		if muster.IsRetryable(sendErr) {
			return out, sendErr
		}
		if muster.IsPermanentRefusal(sendErr) {
			_ = os.Remove(full)
			out.Dropped++
			out.Warnings = append(out.Warnings, sendErr.Error())
			continue
		}
		// Non-retryable 5xx (such as 500 unexpected) or local validation error:
		// drop malformed local entries, keep unexpected server failures for retry
		// only if transient; Muster 500 unexpected/bad_record carry E_PARSE and
		// cannot succeed by replaying the identical payload.
		_ = os.Remove(full)
		out.Dropped++
		out.Warnings = append(out.Warnings, sendErr.Error())
	}
	return out, nil
}
