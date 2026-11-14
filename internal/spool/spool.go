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
	"sync"
	"sync/atomic"
	"syscall"

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
	maxEntries    = 64
	maxMemEntries = 1
	maxEntrySize  = 128 << 10
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

type memRecord struct {
	name string
	raw  []byte
}

// Spool manages staged reports in a single writable directory, or in a bounded
// single-slot in-memory buffer when the container rootfs is read-only without
// a mounted spool volume.
type Spool struct {
	dir      string
	inMemory bool
	seq      atomic.Uint64
	mu       sync.Mutex
	mem      []memRecord
}

// NewMemory returns a bounded single-entry in-memory Spool that applies the
// same validation, canonical JSON encoding, and SHA-256 verification as Open.
func NewMemory() *Spool {
	return &Spool{inMemory: true}
}

// OpenOrMemory opens dir when writable, or falls back to NewMemory (returning
// inMemory=true) only when dir cannot be created or written due to a read-only
// filesystem or directory permissions.
func OpenOrMemory(dir string) (*Spool, bool, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, false, ErrSpoolFormat
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if isReadOnlyOrPerm(err) {
			return NewMemory(), true, nil
		}
		return nil, false, ErrSpoolIO
	}
	probe, err := os.CreateTemp(dir, ".probe-*.tmp")
	if err != nil {
		if isReadOnlyOrPerm(err) {
			return NewMemory(), true, nil
		}
		return nil, false, ErrSpoolIO
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	s, err := Open(dir)
	if err != nil {
		return nil, false, err
	}
	return s, false, nil
}

func isReadOnlyOrPerm(err error) bool {
	return errors.Is(err, syscall.EROFS) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES)
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
// atomically into the spool directory (or into the bounded in-memory slot).
func (s *Spool) Stage(e Entry) (string, error) {
	if err := validateEntry(&e); err != nil {
		return "", err
	}
	digest, err := computeDigest(e)
	if err != nil {
		return "", err
	}
	e.Digest = digest

	raw, err := json.Marshal(e)
	if err != nil || len(raw) > maxEntrySize {
		return "", ErrSpoolFormat
	}
	raw = append(raw, '\n')

	if s.inMemory {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.mem) >= maxMemEntries {
			return "", ErrSpoolFull
		}
		n := s.seq.Add(1)
		finalName := fmt.Sprintf("%08d-%s.json", n, safeLeaseToken(e.Lease))
		s.mem = append(s.mem, memRecord{name: finalName, raw: raw})
		return finalName, nil
	}

	existing, err := s.files()
	if err != nil {
		return "", ErrSpoolIO
	}
	if len(existing) >= maxEntries {
		return "", ErrSpoolFull
	}

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

// Pending returns how many staged entries are currently waiting.
func (s *Spool) Pending() (int, error) {
	if s.inMemory {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.mem), nil
	}
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

func parseSpooledBytes(raw []byte) (Entry, error) {
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
	return parseSpooledBytes(raw)
}

// DrainOutcome summarizes one Drain pass over staged reports.
type DrainOutcome struct {
	Delivered int
	Dropped   int
	Warnings  []string
}

func dispatchEntry(ctx context.Context, disp muster.Dispatcher, e Entry) error {
	switch e.Kind {
	case KindComplete:
		_, err := disp.Complete(ctx, e.Lease, e.State, e.Result)
		return err
	case KindFail:
		_, err := disp.Fail(ctx, e.Lease, e.Code)
		return err
	case KindNeedsSubtool:
		_, err := disp.NeedsSubtool(ctx, e.Lease)
		return err
	default:
		return ErrSpoolFormat
	}
}

// Drain replays all staged reports in order through disp.
//
// An entry acknowledged with 200 OK is deleted. An entry refused permanently
// with a non-retryable 4xx status (for example 409 lease_not_held or
// 409 move_not_allowed) or a corrupted file on disk is dropped and recorded in
// Warnings without filesystem paths. A retryable error stops Drain and leaves
// the remaining entries staged for the next attempt.
func (s *Spool) Drain(ctx context.Context, disp muster.Dispatcher) (DrainOutcome, error) {
	if s.inMemory {
		return s.drainMemory(ctx, disp)
	}
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

		sendErr := dispatchEntry(ctx, disp, e)
		if sendErr == nil {
			_ = os.Remove(full)
			out.Delivered++
			continue
		}
		if muster.IsRetryable(sendErr) {
			return out, sendErr
		}
		_ = os.Remove(full)
		out.Dropped++
		out.Warnings = append(out.Warnings, sendErr.Error())
	}
	return out, nil
}

func (s *Spool) drainMemory(ctx context.Context, disp muster.Dispatcher) (DrainOutcome, error) {
	var out DrainOutcome
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		s.mu.Lock()
		if len(s.mem) == 0 {
			s.mu.Unlock()
			return out, nil
		}
		rec := s.mem[0]
		s.mu.Unlock()

		e, err := parseSpooledBytes(rec.raw)
		if err != nil {
			s.mu.Lock()
			if len(s.mem) > 0 && s.mem[0].name == rec.name {
				s.mem = s.mem[1:]
			}
			s.mu.Unlock()
			out.Dropped++
			out.Warnings = append(out.Warnings, muster.CodeParse+": dropped corrupted spool entry")
			continue
		}

		sendErr := dispatchEntry(ctx, disp, e)
		if sendErr == nil {
			s.mu.Lock()
			if len(s.mem) > 0 && s.mem[0].name == rec.name {
				s.mem = s.mem[1:]
			}
			s.mu.Unlock()
			out.Delivered++
			continue
		}
		if muster.IsRetryable(sendErr) {
			return out, sendErr
		}
		s.mu.Lock()
		if len(s.mem) > 0 && s.mem[0].name == rec.name {
			s.mem = s.mem[1:]
		}
		s.mu.Unlock()
		out.Dropped++
		out.Warnings = append(out.Warnings, sendErr.Error())
	}
}
