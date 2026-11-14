package muster

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"mime"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxWaybillBody = 256 << 10
	maxInFlight    = 64
	maxEditBytes   = 4096
	maxTitleBytes  = 200
	maxAddrBytes   = 2000
	maxHealthBatch = 64
	checkEveryDays = 7
)

type problem struct {
	Status int
	Code   string
	Cause  string
	Reason string
}

func (p *problem) Error() string {
	return p.Code + " " + p.Cause + ": " + p.Reason
}

var (
	probNotJSON           = &problem{http.StatusBadRequest, CodeParse, "not_json", "the request is not one json value"}
	probUnknownField      = &problem{http.StatusBadRequest, CodeParse, "unknown_field", "the request has a field this call does not define"}
	probMissingField      = &problem{http.StatusBadRequest, CodeParse, "missing_field", "a required field is missing"}
	probBadField          = &problem{http.StatusBadRequest, CodeParse, "bad_field", "a field does not have the form this call requires"}
	probUseNeedsSubtool   = &problem{http.StatusBadRequest, CodeParse, "bad_field", "report a missing rule with the needs_subtool call"}
	probBadProof          = &problem{http.StatusBadRequest, CodeParse, "bad_field", "the proof of work is not valid"}
	probUnknownTarget     = &problem{http.StatusBadRequest, CodeParse, "bad_field", "the target is not in the ownership map"}
	probNothingLeft       = &problem{http.StatusBadRequest, CodeParse, "bad_field", "nothing is left of the text once markup is removed"}
	probLogbookRefused    = &problem{http.StatusBadRequest, CodeParse, "bad_field", "the record store refused a field"}
	probNoCredential      = &problem{http.StatusUnauthorized, CodeParse, "no_credential", "no valid credential was presented"}
	probNotYourCall       = &problem{http.StatusForbidden, CodeParse, "not_your_call", "this caller may not make this call"}
	probNoSuchCall        = &problem{http.StatusNotFound, CodeParse, "no_such_call", "no such call"}
	probWrongMethod       = &problem{http.StatusMethodNotAllowed, CodeParse, "wrong_method", "this call does not accept that method"}
	probTooLarge          = &problem{http.StatusRequestEntityTooLarge, CodeParse, "too_large", "the request is too large"}
	probWrongType         = &problem{http.StatusUnsupportedMediaType, CodeParse, "wrong_type", "the content type must be application/json"}
	probLeaseNotHeld      = &problem{http.StatusConflict, CodeParse, "lease_not_held", "the lease is not held"}
	probMoveNotAllowed    = &problem{http.StatusConflict, CodeParse, "move_not_allowed", "that move is not allowed"}
	probDuplicate         = &problem{http.StatusConflict, CodeParse, "duplicate", "the same component is reported twice in one batch"}
	probUnknownContract   = &problem{http.StatusUnprocessableEntity, CodeNeedsSubtool, "unknown_contract", "this contract version is not known"}
	probUnknownHealth     = &problem{http.StatusUnprocessableEntity, CodeNeedsSubtool, "unknown_value", "this health state is not known"}
	probUnknownSource     = &problem{http.StatusUnprocessableEntity, CodeNeedsSubtool, "unknown_value", "this source is not known"}
	probLogbookUnfamiliar = &problem{http.StatusUnprocessableEntity, CodeNeedsSubtool, "unknown_value", "the record store has no rule for a value"}
	probRateLimited       = &problem{http.StatusTooManyRequests, CodeTimeout, "rate_limited", "too many submissions from this visitor today; try again tomorrow"}
	probStoreSlow         = &problem{http.StatusServiceUnavailable, CodeTimeout, "store_slow", "the record store took too long"}
	probBusy              = &problem{http.StatusServiceUnavailable, CodeTimeout, "busy", "too busy; try again"}
	probIdentitySlow      = &problem{http.StatusServiceUnavailable, CodeTimeout, "busy", "the identity check took too long; try again"}
	probBadRecord         = &problem{http.StatusInternalServerError, CodeParse, "bad_record", "a stored record was not consistent and has been set aside"}
	probUnexpected        = &problem{http.StatusInternalServerError, CodeParse, "unexpected", "an unexpected condition; the request was refused"}
)

var allProblems = []*problem{
	probNotJSON, probUnknownField, probMissingField, probBadField, probUseNeedsSubtool, probBadProof,
	probUnknownTarget, probNothingLeft, probLogbookRefused, probNoCredential, probNotYourCall, probNoSuchCall,
	probWrongMethod, probTooLarge, probWrongType, probLeaseNotHeld, probMoveNotAllowed, probDuplicate,
	probUnknownContract, probUnknownHealth, probUnknownSource, probLogbookUnfamiliar, probRateLimited, probStoreSlow,
	probBusy, probIdentitySlow, probBadRecord, probUnexpected,
}

type regEntry struct {
	slug   string
	worker bool
	repo   string
}

var registryTable = map[string]regEntry{
	"conn":        {"conn", false, "clearedstack.com"},
	"muster":      {"muster", false, "muster"},
	"logbook":     {"logbook", false, "logbook"},
	"plainforge":  {"plainforge", false, "plainforge"},
	"lazarette":   {"lazarette", false, "lazarette"},
	"keelworks":   {"keelworks", false, "keelworks"},
	"shipkeeper":  {"shipkeeper", false, "shipkeeper"},
	"trawl":       {"trawl", true, "trawl"},
	"sextant":     {"sextant", true, "sextant"},
	"assay":       {"assay", true, "assay"},
	"countersign": {"countersign", true, "countersign"},
	"waybill":     {"waybill", true, "waybill"},
	"moldloft":    {"moldloft", true, "moldloft"},
	"shakedown":   {"shakedown", true, "shakedown"},
	"chartroom":   {"chartroom", true, "chartroom"},
	"tidegauge":   {"tidegauge", true, "tidegauge"},
	"bellbuoy":    {"bellbuoy", true, "bellbuoy"},
}

type routeRow struct {
	worker    string
	kind      string
	from      string
	to        string
	next      string
	lane      string
	afterDays int
}

var routeTable = []routeRow{
	{"trawl", "item", "discovered", "recorded", "sextant", "", 0},
	{"trawl", "item", "discovered", "rejected", "", "", 0},
	{"sextant", "item", "recorded", "observed", "assay", "", 0},
	{"assay", "item", "observed", "scored", "countersign", "", 0},
	{"assay", "item", "observed", "screened_out", "countersign", "", 0},
	{"countersign", "item", "scored", "approved", "waybill", "", 0},
	{"countersign", "item", "scored", "parked", "", "", 0},
	{"countersign", "item", "scored", "linked", "chartroom", "", 0},
	{"countersign", "item", "scored", "merged", "chartroom", "", 0},
	{"countersign", "item", "scored", "rejected", "", "", 0},
	{"countersign", "item", "screened_out", "observed", "assay", "", 0},
	{"waybill", "item", "approved", "approved", "moldloft", "", 0},
	{"moldloft", "item", "approved", "building", "shakedown", "", 0},
	{"shakedown", "item", "building", "building", "chartroom", "", 0},
	{"shakedown", "item", "building", "parked", "", "", 0},
	{"chartroom", "item", "building", "live", "tidegauge", "backfill", checkEveryDays},
	{"chartroom", "item", "linked", "linked", "", "", 0},
	{"chartroom", "item", "merged", "merged", "", "", 0},
	{"chartroom", "item", "retired", "retired", "", "", 0},
	{"tidegauge", "item", "live", "live", "tidegauge", "backfill", checkEveryDays},
	{"tidegauge", "item", "live", "retired", "chartroom", "urgent", 0},
	{"tidegauge", "item", "live", "building", "waybill", "urgent", 0},
	{"waybill", "item", "building", "building", "moldloft", "", 0},
	{"moldloft", "item", "building", "building", "shakedown", "", 0},
	{"countersign", "proposal", "pending_approval", "approved", "bellbuoy", "urgent", 0},
	{"countersign", "proposal", "pending_approval", "declined", "", "", 0},
	{"bellbuoy", "proposal", "approved", "building", "", "", 0},
}

var itemMovesSet = map[[2]string]bool{
	{"discovered", "recorded"}:     true,
	{"discovered", "rejected"}:     true,
	{"recorded", "observed"}:       true,
	{"recorded", "needs_subtool"}:  true,
	{"observed", "scored"}:         true,
	{"observed", "screened_out"}:   true,
	{"observed", "needs_subtool"}:  true,
	{"screened_out", "observed"}:   true,
	{"scored", "approved"}:         true,
	{"scored", "parked"}:           true,
	{"scored", "linked"}:           true,
	{"scored", "merged"}:           true,
	{"scored", "rejected"}:         true,
	{"approved", "building"}:       true,
	{"approved", "needs_subtool"}:  true,
	{"building", "live"}:           true,
	{"building", "parked"}:         true,
	{"live", "building"}:           true,
	{"live", "retired"}:            true,
	{"parked", "observed"}:         true,
	{"linked", "observed"}:         true,
	{"merged", "observed"}:         true,
	{"needs_subtool", "recorded"}:  true,
	{"needs_subtool", "observed"}:  true,
	{"needs_subtool", "approved"}:  true,
}

var proposalMovesSet = map[[2]string]bool{
	{"pending_approval", "approved"}: true,
	{"pending_approval", "declined"}: true,
	{"approved", "building"}:         true,
	{"building", "live"}:             true,
}

var knownItemStates = map[string]bool{
	"discovered": true, "recorded": true, "observed": true, "screened_out": true,
	"scored": true, "approved": true, "building": true, "live": true,
	"parked": true, "linked": true, "merged": true, "rejected": true,
	"retired": true, "needs_subtool": true,
}

func laneRank(lane string) int {
	switch lane {
	case "urgent":
		return 0
	case "normal":
		return 1
	case "backfill":
		return 2
	default:
		return -1
	}
}

func findRoute(worker, kind, from, to string) (routeRow, bool) {
	for _, r := range routeTable {
		if r.worker == worker && r.kind == kind && r.from == from && r.to == to {
			return r, true
		}
	}
	return routeRow{}, false
}

func workerHolds(worker, kind, state string) bool {
	for _, r := range routeTable {
		if r.worker == worker && r.kind == kind && r.from == state {
			return true
		}
	}
	return false
}

var (
	itemIDRE     = regexp.MustCompile(`^PA-(?:[0-9]{4}|[1-9][0-9]{4,8})$`)
	leaseFormRE  = regexp.MustCompile(`^((?:incident:)?PA-(?:[0-9]{4}|[1-9][0-9]{4,8})|EP-(?:[0-9]{4}|[1-9][0-9]{4,8}))\.[1-9][0-9]{0,18}\.[1-9][0-9]{0,8}$`)
	digestFormRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	powTokenRE   = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})\.([0-9a-f]{1,32})$`)
)

// SeedItem seeds an initial item into StandIn.
type SeedItem struct {
	ID      string                     `json:"id"`
	State   string                     `json:"state"`
	Lane    string                     `json:"lane"`
	Next    string                     `json:"next,omitempty"`
	Results map[string]json.RawMessage `json:"results,omitempty"`
}

// OwnerEntry defines one editable target in StandIn's ownership map.
type OwnerEntry struct {
	Repository string `json:"repository"`
	File       string `json:"file"`
	Kind       string `json:"kind"`
}

type finishedCall struct {
	lease  string
	worker string
	call   string
	arg    string
	ack    Ack
}

// ItemRecord is StandIn's stored view of an item.
type ItemRecord struct {
	ID           string
	State        string
	Lane         string
	Source       string
	Title        string
	Address      string
	PausedFrom   string
	PausedBy     string
	PausedSeq    uint64
	Results      map[string]json.RawMessage
	Done         finishedCall
	IncidentDone finishedCall
}

type proposalRecord struct {
	ID         string
	Target     string
	Repository string
	File       string
	EditKind   string
	Change     string
	Reason     string
	Status     string
	Received   time.Time
	Results    map[string]json.RawMessage
	Done       finishedCall
}

type queueEntry struct {
	Key       string
	Subject   string
	Kind      string
	Worker    string
	Lane      string
	Arrival   uint64
	Attempt   int
	Lease     string
	Expires   time.Time
	NotBefore time.Time
	Held      bool
	Incident  IncidentView
}

type componentRecord struct {
	Slug   string
	State  string
	Code   string
	Digest string
	At     time.Time
}

// StandIn is an in-memory HTTP stand-in for Muster speaking contract v1.
type StandIn struct {
	mu sync.Mutex

	now                time.Time
	leaseFor           time.Duration
	powBits            int
	dailyCap           int
	saltSecret         []byte
	owners             map[string]OwnerEntry
	tokenToCaller      map[string]string
	callerToToken      map[string]string
	allowPauseBuilding bool

	// Simulated fault conditions for contract example replay.
	condSlowAuth      bool
	condBusy          bool
	condStoreErr      error
	condSubscriberCap bool

	items      map[string]ItemRecord
	proposals  map[string]proposalRecord
	entries    map[string]queueEntry
	addresses  map[string]string
	components map[string]componentRecord
	counters   map[string]uint64
	countDay   string
	counts     map[string]int

	nextEventID uint64
	subs        map[chan []byte]struct{}
	inFlight    chan struct{}
}

// NewStandIn returns a Muster contract v1 stand-in starting at 2026-01-05T09:00:00Z.
func NewStandIn() *StandIn {
	t2c := map[string]string{}
	c2t := map[string]string{}
	for slug := range registryTable {
		tok := "test-token-" + slug
		t2c[tok] = slug
		c2t[slug] = tok
	}
	return &StandIn{
		now:           time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC),
		leaseFor:      60 * time.Second,
		powBits:       20,
		dailyCap:      5,
		saltSecret:    []byte("made-up-salt-secret-for-tests-only-0123456789abcdef"),
		tokenToCaller: t2c,
		callerToToken: c2t,
		owners: map[string]OwnerEntry{
			"catalog.sample-calculator.description": {
				Repository: "clearedstack.com",
				File:       "src/data/sample-catalog.json",
				Kind:       "DATA",
			},
			"sextant.sample-portal.options": {
				Repository: "sextant",
				File:       "tools/sample-portal/options.json",
				Kind:       "CONFIG",
			},
			"assay.sample-profile.report": {
				Repository: "assay",
				File:       "tools/sample-profile/README.md",
				Kind:       "ISSUE",
			},
		},
		items:      map[string]ItemRecord{},
		proposals:  map[string]proposalRecord{},
		entries:    map[string]queueEntry{},
		addresses:  map[string]string{},
		components: map[string]componentRecord{},
		counters:   map[string]uint64{},
		counts:     map[string]int{},
		subs:       map[chan []byte]struct{}{},
		inFlight:   make(chan struct{}, maxInFlight),
	}
}

// TokenFor returns the synthetic test bearer token registered for caller.
func (s *StandIn) TokenFor(caller string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callerToToken[caller]
}

// AdvanceClock moves StandIn's clock forward by d.
func (s *StandIn) AdvanceClock(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

// Now returns StandIn's current clock value.
func (s *StandIn) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// SetAllowPauseBuilding toggles whether StandIn allows POST /v1/needs_subtool
// on items in the building state (false by default per CONTRACT.md).
func (s *StandIn) SetAllowPauseBuilding(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowPauseBuilding = v
}

// Seed loads initial items into StandIn in order.
func (s *StandIn) Seed(items []SeedItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var highest uint64
	for i, it := range items {
		if !itemIDRE.MatchString(it.ID) {
			return fmt.Errorf("seed item %d: bad id", i+1)
		}
		if n, _ := strconv.ParseUint(it.ID[len("PA-"):], 10, 64); n > highest {
			highest = n
		}
		if _, dup := s.items[it.ID]; dup {
			return fmt.Errorf("seed item %s: duplicate", it.ID)
		}
		if !knownItemStates[it.State] || it.State == "needs_subtool" {
			return fmt.Errorf("seed item %s: invalid state", it.ID)
		}
		if laneRank(it.Lane) < 0 {
			return fmt.Errorf("seed item %s: invalid lane", it.ID)
		}
		next, lane := it.Next, it.Lane
		if next == "" && it.State == "live" {
			next, lane = "tidegauge", "backfill"
		}
		var res map[string]json.RawMessage
		if len(it.Results) > 0 {
			res = make(map[string]json.RawMessage, len(it.Results))
			for k, v := range it.Results {
				res[k] = append(json.RawMessage(nil), v...)
			}
		}
		s.items[it.ID] = ItemRecord{
			ID:      it.ID,
			State:   it.State,
			Lane:    lane,
			Results: res,
		}
		if next != "" {
			if !workerHolds(next, "item", it.State) {
				return fmt.Errorf("seed item %s: worker %s cannot hold %s", it.ID, next, it.State)
			}
			s.counters["arrival"]++
			s.entries[it.ID] = queueEntry{
				Key:     it.ID,
				Subject: it.ID,
				Kind:    "item",
				Worker:  next,
				Lane:    lane,
				Arrival: s.counters["arrival"],
			}
		}
	}
	if highest > s.counters["item"] {
		s.counters["item"] = highest
	}
	return nil
}

// Item returns a snapshot of a stored item and whether it is currently held in queue.
func (s *StandIn) Item(id string) (ItemRecord, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return ItemRecord{}, false, false
	}
	cp := it
	if len(it.Results) > 0 {
		cp.Results = make(map[string]json.RawMessage, len(it.Results))
		for k, v := range it.Results {
			cp.Results[k] = append(json.RawMessage(nil), v...)
		}
	}
	e, queued := s.entries[id]
	return cp, true, queued && e.Held
}

// NextWorker returns the worker currently queued on id, or "" if unqueued.
func (s *StandIn) NextWorker(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || e.Held {
		return ""
	}
	return e.Worker
}

// Arrange configures one of the simulated conditions named in contract/v1/examples.
func (s *StandIn) Arrange(condition string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.condSlowAuth = false
	s.condBusy = false
	s.condStoreErr = nil
	s.condSubscriberCap = false
	switch condition {
	case "":
	case "logbook takes too long":
		s.condStoreErr = probStoreSlow
	case "logbook fails in a way nothing provides for":
		s.condStoreErr = probUnexpected
	case "every place for a call in flight is taken":
		s.condBusy = true
	case "the cluster does not answer the identity check in time":
		s.condSlowAuth = true
	case "logbook refuses a field":
		s.condStoreErr = probLogbookRefused
	case "logbook has no rule for a value":
		s.condStoreErr = probLogbookUnfamiliar
	case "the queue says sextant is next for an item that is live":
		s.items["PA-0001"] = ItemRecord{ID: "PA-0001", State: "live", Lane: "normal"}
		s.counters["arrival"]++
		s.entries["PA-0001"] = queueEntry{
			Key:     "PA-0001",
			Subject: "PA-0001",
			Kind:    "item",
			Worker:  "sextant",
			Lane:    "normal",
			Arrival: s.counters["arrival"],
		}
	default:
		return fmt.Errorf("unknown condition %q", condition)
	}
	return nil
}

type routeSpec struct {
	method string
	may    func(string) bool
	stream bool
	handle func(*StandIn, http.ResponseWriter, *http.Request, string)
}

func allowOnly(names ...string) func(string) bool {
	return func(caller string) bool {
		for _, n := range names {
			if caller == n {
				return true
			}
		}
		return false
	}
}

func allowWorker(caller string) bool {
	e, ok := registryTable[caller]
	return ok && e.worker
}

var standInRoutes = map[string]routeSpec{
	"/healthz":          {method: http.MethodGet, handle: (*StandIn).handleAlive},
	"/v1/lease":         {method: http.MethodPost, may: allowWorker, handle: (*StandIn).handleLease},
	"/v1/heartbeat":     {method: http.MethodPost, may: allowWorker, handle: (*StandIn).handleHeartbeat},
	"/v1/complete":      {method: http.MethodPost, may: allowWorker, handle: (*StandIn).handleComplete},
	"/v1/fail":          {method: http.MethodPost, may: allowWorker, handle: (*StandIn).handleFail},
	"/v1/needs_subtool": {method: http.MethodPost, may: allowWorker, handle: (*StandIn).handleNeedsSubtool},
	"/v1/release":       {method: http.MethodPost, may: allowOnly("countersign"), handle: (*StandIn).handleRelease},
	"/v1/items":         {method: http.MethodPost, may: allowOnly("trawl", "conn"), handle: (*StandIn).handleItems},
	"/v1/health":        {method: http.MethodPost, may: allowOnly("shipkeeper"), handle: (*StandIn).handleHealth},
	"/v1/edits":         {method: http.MethodPost, may: allowOnly("conn"), handle: (*StandIn).handleEdits},
	"/v1/feed":          {method: http.MethodGet, may: allowOnly("conn"), stream: true, handle: (*StandIn).handleFeed},
}

// ServeHTTP implements http.Handler for Muster's contract v1 surface.
func (s *StandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spec, ok := standInRoutes[r.URL.Path]
	if !ok {
		writeProblem(w, probNoSuchCall)
		return
	}
	if r.Method != spec.method {
		w.Header().Set("Allow", spec.method)
		writeProblem(w, probWrongMethod)
		return
	}
	if spec.may == nil {
		spec.handle(s, w, r, "")
		return
	}
	if !spec.stream {
		s.mu.Lock()
		busy := s.condBusy
		s.mu.Unlock()
		if busy {
			writeProblem(w, probBusy)
			return
		}
		select {
		case s.inFlight <- struct{}{}:
			defer func() { <-s.inFlight }()
		default:
			writeProblem(w, probBusy)
			return
		}
	}
	caller, prob := s.authenticate(r, spec.may)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	spec.handle(s, w, r, caller)
}

func (s *StandIn) authenticate(r *http.Request, may func(string) bool) (string, *problem) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", probNoCredential
	}
	s.mu.Lock()
	slow := s.condSlowAuth
	caller, known := s.tokenToCaller[tok]
	s.mu.Unlock()
	if slow {
		return "", probIdentitySlow
	}
	if !known {
		return "", probNoCredential
	}
	if !may(caller) {
		return "", probNotYourCall
	}
	return caller, nil
}

func (s *StandIn) handleAlive(w http.ResponseWriter, _ *http.Request, _ string) {
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func readContractBody(w http.ResponseWriter, r *http.Request, limit int64, dst any, version *string) (int, *problem) {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		return 0, probWrongType
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return 0, probTooLarge
		}
		return 0, probNotJSON
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var wrongType *json.UnmarshalTypeError
		switch {
		case errors.As(err, &wrongType):
			return 0, probBadField
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return 0, probUnknownField
		default:
			return 0, probNotJSON
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, probNotJSON
	}
	switch *version {
	case Version:
		return len(body), nil
	case "":
		return 0, probMissingField
	default:
		return 0, probUnknownContract
	}
}

func (s *StandIn) checkStoreLocked() *problem {
	if s.condStoreErr == nil {
		return nil
	}
	var p *problem
	if errors.As(s.condStoreErr, &p) {
		return p
	}
	return probUnexpected
}

func entryWaiting(e queueEntry, now time.Time) bool {
	return !e.Held && !e.NotBefore.After(now) && (e.Lease == "" || !e.Expires.After(now))
}

func entryEarlier(a, b queueEntry) bool {
	ra, rb := laneRank(a.Lane), laneRank(b.Lane)
	if ra != rb {
		return ra < rb
	}
	return a.Arrival < b.Arrival
}

func (s *StandIn) stepStateLocked(e queueEntry) (string, bool) {
	switch e.Kind {
	case "item":
		it, ok := s.items[e.Subject]
		return it.State, ok
	case "proposal":
		p, ok := s.proposals[e.Subject]
		return p.Status, ok
	case "incident":
		_, ok := s.items[e.Subject]
		return e.Incident.State, ok
	default:
		return "", false
	}
}

func mayHoldEntry(worker string, e queueEntry, state string) bool {
	if e.Kind == "incident" {
		return worker == "bellbuoy"
	}
	return workerHolds(worker, e.Kind, state)
}

func (s *StandIn) openIncidentLocked(itemID, worker, code, state string) {
	key := "incident:" + itemID
	s.counters["arrival"]++
	s.entries[key] = queueEntry{
		Key:     key,
		Subject: itemID,
		Kind:    "incident",
		Worker:  "bellbuoy",
		Lane:    "normal",
		Arrival: s.counters["arrival"],
		Incident: IncidentView{
			ID:     itemID,
			Worker: worker,
			Code:   code,
			State:  state,
		},
	}
	s.counters["ledger"]++
}

func (s *StandIn) handleLease(w http.ResponseWriter, r *http.Request, caller string) {
	var req leaseWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	var best queueEntry
	found := false
	for _, e := range s.entries {
		if e.Worker != caller || !entryWaiting(e, s.now) {
			continue
		}
		if !found || entryEarlier(e, best) {
			best, found = e, true
		}
	}
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	state, ok := s.stepStateLocked(best)
	if !ok || !mayHoldEntry(caller, best, state) {
		best.Held, best.Lease, best.Expires = true, "", time.Time{}
		s.entries[best.Key] = best
		s.counters["ledger"]++
		if _, isItem := s.items[best.Subject]; isItem && best.Kind == "item" {
			s.openIncidentLocked(best.Subject, caller, CodeParse, state)
		}
		writeProblem(w, probBadRecord)
		return
	}
	best.Attempt++
	best.Lease = best.Key + "." + strconv.FormatUint(best.Arrival, 10) + "." + strconv.Itoa(best.Attempt)
	best.Expires = s.now.Add(s.leaseFor)
	s.entries[best.Key] = best
	s.counters["ledger"]++

	grant := Grant{
		Lease:   best.Lease,
		Kind:    best.Kind,
		ID:      best.Subject,
		State:   state,
		Lane:    best.Lane,
		Attempt: best.Attempt,
		Expires: best.Expires.UTC().Format(time.RFC3339),
	}
	switch best.Kind {
	case "item":
		it := s.items[best.Subject]
		if len(it.Results) > 0 {
			grant.Results = make(map[string]json.RawMessage, len(it.Results))
			for k, v := range it.Results {
				grant.Results[k] = append(json.RawMessage(nil), v...)
			}
		}
		if it.Address != "" {
			grant.Found = &FoundView{Source: it.Source, Title: it.Title, Address: it.Address}
		}
	case "proposal":
		p := s.proposals[best.Subject]
		grant.Results = p.Results
		grant.Proposal = &ProposalView{
			Target: p.Target, Repository: p.Repository, File: p.File,
			EditKind: p.EditKind, Change: p.Change, Reason: p.Reason,
		}
	case "incident":
		inc := best.Incident
		grant.Incident = &inc
	}
	writeJSON(w, http.StatusOK, grant)
}

func parseLeaseKey(lease string) (string, bool) {
	if !leaseFormRE.MatchString(lease) {
		return "", false
	}
	return lease[:strings.IndexByte(lease, '.')], true
}

func (s *StandIn) heldEntryLocked(worker, lease string) (queueEntry, bool) {
	key, ok := parseLeaseKey(lease)
	if !ok {
		return queueEntry{}, false
	}
	e, ok := s.entries[key]
	if !ok || e.Lease != lease || e.Worker != worker {
		return queueEntry{}, false
	}
	return e, true
}

func (s *StandIn) repeatedCallLocked(worker, lease, call, arg string) (*Ack, *problem) {
	key, ok := parseLeaseKey(lease)
	if !ok {
		return nil, probLeaseNotHeld
	}
	var done finishedCall
	if id, isInc := strings.CutPrefix(key, "incident:"); isInc {
		if it, ok := s.items[id]; ok {
			done = it.IncidentDone
		}
	} else if it, ok := s.items[key]; ok {
		done = it.Done
	} else if p, ok := s.proposals[key]; ok {
		done = p.Done
	}
	if done.lease != lease || done.worker != worker || done.call != call || done.arg != arg {
		return nil, probLeaseNotHeld
	}
	ack := done.ack
	return &ack, nil
}

func (s *StandIn) handleHeartbeat(w http.ResponseWriter, r *http.Request, caller string) {
	var req leaseIDWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	if req.Lease == "" {
		writeProblem(w, probMissingField)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	e, ok := s.heldEntryLocked(caller, req.Lease)
	if !ok {
		writeProblem(w, probLeaseNotHeld)
		return
	}
	e.Expires = s.now.Add(s.leaseFor)
	s.entries[e.Key] = e
	writeJSON(w, http.StatusOK, Beat{
		Lease:   e.Lease,
		Expires: e.Expires.UTC().Format(time.RFC3339),
	})
}

func optionalObject(raw json.RawMessage) (json.RawMessage, bool) {
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return nil, true
	case raw[0] != '{':
		return nil, false
	default:
		return raw, true
	}
}

func (s *StandIn) handleComplete(w http.ResponseWriter, r *http.Request, caller string) {
	var req completeWire
	limit := int64(MaxBody)
	if caller == "waybill" {
		limit = maxWaybillBody
	}
	if _, p := readContractBody(w, r, limit, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	if req.Lease == "" || req.State == "" {
		writeProblem(w, probMissingField)
		return
	}
	result, ok := optionalObject(req.Result)
	if !ok {
		writeProblem(w, probBadField)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	e, ok := s.heldEntryLocked(caller, req.Lease)
	if !ok {
		ack, prob := s.repeatedCallLocked(caller, req.Lease, "complete", req.State)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		writeJSON(w, http.StatusOK, ack)
		return
	}

	if e.Kind == "incident" {
		if req.State != e.Incident.State {
			writeProblem(w, probMoveNotAllowed)
			return
		}
		if result != nil {
			writeProblem(w, probBadField)
			return
		}
		ack := Ack{ID: e.Subject, State: req.State}
		if it, ok := s.items[e.Subject]; ok {
			it.IncidentDone = finishedCall{lease: req.Lease, worker: caller, call: "complete", arg: req.State, ack: ack}
			s.items[e.Subject] = it
		}
		delete(s.entries, e.Key)
		s.counters["ledger"]++
		writeJSON(w, http.StatusOK, ack)
		return
	}

	from, _ := s.stepStateLocked(e)
	route, ok := findRoute(caller, e.Kind, from, req.State)
	if !ok {
		writeProblem(w, probMoveNotAllowed)
		return
	}
	if from != req.State {
		allowed := false
		if e.Kind == "item" {
			allowed = itemMovesSet[[2]string{from, req.State}]
		} else if e.Kind == "proposal" {
			allowed = proposalMovesSet[[2]string{from, req.State}]
		}
		if !allowed {
			writeProblem(w, probMoveNotAllowed)
			return
		}
	}
	lane := e.Lane
	if route.lane != "" {
		lane = route.lane
	}
	ack := Ack{ID: e.Subject, State: req.State, Next: route.next}
	done := finishedCall{lease: req.Lease, worker: caller, call: "complete", arg: req.State, ack: ack}

	switch e.Kind {
	case "item":
		it := s.items[e.Subject]
		it.State, it.Lane, it.Done = req.State, lane, done
		if result != nil {
			if it.Results == nil {
				it.Results = map[string]json.RawMessage{}
			}
			it.Results[caller] = append(json.RawMessage(nil), result...)
		}
		s.items[e.Subject] = it
	case "proposal":
		p := s.proposals[e.Subject]
		p.Status, p.Done = req.State, done
		if result != nil {
			if p.Results == nil {
				p.Results = map[string]json.RawMessage{}
			}
			p.Results[caller] = append(json.RawMessage(nil), result...)
		}
		s.proposals[e.Subject] = p
	}

	if route.next == "" {
		delete(s.entries, e.Key)
	} else {
		s.counters["arrival"]++
		next := queueEntry{
			Key:     e.Key,
			Subject: e.Subject,
			Kind:    e.Kind,
			Worker:  route.next,
			Lane:    lane,
			Arrival: s.counters["arrival"],
		}
		if route.afterDays > 0 {
			next.NotBefore = s.now.AddDate(0, 0, route.afterDays).UTC()
		}
		s.entries[e.Key] = next
	}
	s.counters["ledger"]++
	writeJSON(w, http.StatusOK, ack)
}

func (s *StandIn) handleFail(w http.ResponseWriter, r *http.Request, caller string) {
	var req failWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	if req.Lease == "" || req.Code == "" {
		writeProblem(w, probMissingField)
		return
	}
	switch req.Code {
	case CodeTimeout, CodeParse:
	case CodeNeedsSubtool:
		writeProblem(w, probUseNeedsSubtool)
		return
	default:
		writeProblem(w, probBadField)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	e, ok := s.heldEntryLocked(caller, req.Lease)
	if !ok {
		ack, prob := s.repeatedCallLocked(caller, req.Lease, "fail", req.Code)
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		writeJSON(w, http.StatusOK, ack)
		return
	}
	state, _ := s.stepStateLocked(e)
	e.Lease, e.Expires = "", time.Time{}
	ack := Ack{ID: e.Subject, State: state}
	if req.Code == CodeParse {
		e.Held = true
		ack.Held = true
	} else {
		ack.Next = e.Worker
	}
	s.entries[e.Key] = e
	done := finishedCall{lease: req.Lease, worker: caller, call: "fail", arg: req.Code, ack: ack}
	switch e.Kind {
	case "item":
		if it, ok := s.items[e.Subject]; ok {
			it.Done = done
			s.items[e.Subject] = it
		}
	case "incident":
		if it, ok := s.items[e.Subject]; ok {
			it.IncidentDone = done
			s.items[e.Subject] = it
		}
	case "proposal":
		if p, ok := s.proposals[e.Subject]; ok {
			p.Done = done
			s.proposals[e.Subject] = p
		}
	}
	s.counters["ledger"]++
	if req.Code == CodeParse && e.Kind == "item" {
		s.openIncidentLocked(e.Subject, caller, CodeParse, state)
	}
	writeJSON(w, http.StatusOK, ack)
}

func (s *StandIn) handleNeedsSubtool(w http.ResponseWriter, r *http.Request, caller string) {
	var req leaseIDWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	if req.Lease == "" {
		writeProblem(w, probMissingField)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	e, ok := s.heldEntryLocked(caller, req.Lease)
	if !ok {
		ack, prob := s.repeatedCallLocked(caller, req.Lease, "needs_subtool", "")
		if prob != nil {
			writeProblem(w, prob)
			return
		}
		writeJSON(w, http.StatusOK, ack)
		return
	}
	if e.Kind != "item" {
		writeProblem(w, probMoveNotAllowed)
		return
	}
	it := s.items[e.Subject]
	pausable := itemMovesSet[[2]string{it.State, "needs_subtool"}] || (s.allowPauseBuilding && it.State == "building")
	if !pausable {
		writeProblem(w, probMoveNotAllowed)
		return
	}
	from := it.State
	ack := Ack{ID: it.ID, State: "needs_subtool"}
	s.counters["ledger"]++
	seq := s.counters["ledger"]
	it.State = "needs_subtool"
	it.PausedFrom, it.PausedBy, it.PausedSeq = from, caller, seq
	it.Done = finishedCall{lease: req.Lease, worker: caller, call: "needs_subtool", ack: ack}
	s.items[it.ID] = it
	delete(s.entries, e.Key)
	s.openIncidentLocked(it.ID, caller, CodeNeedsSubtool, from)
	writeJSON(w, http.StatusOK, ack)
}

func (s *StandIn) handleRelease(w http.ResponseWriter, r *http.Request, _ string) {
	var req releaseWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	if req.ID == "" {
		writeProblem(w, probMissingField)
		return
	}
	if !itemIDRE.MatchString(req.ID) {
		writeProblem(w, probBadField)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	it, ok := s.items[req.ID]
	e, queued := s.entries[req.ID]
	if !ok || !queued || !e.Held {
		writeProblem(w, probMoveNotAllowed)
		return
	}
	s.counters["arrival"]++
	e.Held, e.Lease, e.Expires = false, "", time.Time{}
	e.Lane, e.Arrival = "urgent", s.counters["arrival"]
	s.entries[req.ID] = e
	it.Lane = "urgent"
	s.items[req.ID] = it
	s.counters["ledger"]++
	writeJSON(w, http.StatusOK, Ack{ID: req.ID, State: it.State, Next: e.Worker})
}

type itemWire struct {
	Contract string          `json:"contract"`
	Source   string          `json:"source"`
	Title    string          `json:"title"`
	Address  string          `json:"address"`
	Result   json.RawMessage `json:"result"`
	Pow      string          `json:"pow"`
	Visitor  string          `json:"visitor"`
}

type recordedWire struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Next      string `json:"next,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

var itemSources = map[string]string{
	"sam-gov":       "normal",
	"github-issues": "normal",
	"web-sweep":     "backfill",
	"site-form":     "normal",
}

func validTextField(s string) bool {
	return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}

func plainField(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func unbrokenField(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validSourceForm(s string) bool {
	if len(s) > 32 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func parseVisitorAddr(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr, true
}

func verifyPow(token, a, b, c string, difficulty int, now time.Time) bool {
	m := powTokenRE.FindStringSubmatch(token)
	if m == nil {
		return false
	}
	day, nonce := m[1], m[2]
	today := now.UTC()
	if day != today.Format("2006-01-02") && day != today.AddDate(0, 0, -1).Format("2006-01-02") {
		return false
	}
	h := sha256.New()
	for _, part := range []string{"muster-pow-v1", day, a, b, c} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write([]byte(nonce))
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	zeros := 0
	for _, by := range sum {
		if by != 0 {
			zeros += bits.LeadingZeros8(by)
			break
		}
		zeros += 8
	}
	return zeros >= difficulty
}

func (s *StandIn) admitVisitorLocked(addr netip.Addr) bool {
	day := s.now.UTC().Format("2006-01-02")
	if s.countDay != day {
		s.countDay = day
		s.counts = map[string]int{}
	}
	salt := hmac.New(sha256.New, s.saltSecret)
	salt.Write([]byte("muster-salt-v1\n" + day))
	mac := hmac.New(sha256.New, salt.Sum(nil))
	mac.Write([]byte(addr.Unmap().String()))
	key := hex.EncodeToString(mac.Sum(nil))
	s.counts[key]++
	return s.counts[key] <= s.dailyCap
}

func (s *StandIn) handleItems(w http.ResponseWriter, r *http.Request, caller string) {
	var req itemWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	result, ok := optionalObject(req.Result)
	if !ok {
		writeProblem(w, probBadField)
		return
	}
	for _, f := range []string{req.Source, req.Title, req.Address, req.Pow, req.Visitor} {
		if !validTextField(f) {
			writeProblem(w, probBadField)
			return
		}
	}
	if req.Source == "" || req.Title == "" || req.Address == "" {
		writeProblem(w, probMissingField)
		return
	}
	if !validSourceForm(req.Source) || len(req.Title) > maxTitleBytes || !plainField(req.Title) || len(req.Address) > maxAddrBytes || !unbrokenField(req.Address) {
		writeProblem(w, probBadField)
		return
	}
	lane, known := itemSources[req.Source]
	if !known {
		writeProblem(w, probUnknownSource)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fromForm := req.Source == "site-form"
	var addr netip.Addr
	if fromForm {
		if req.Pow == "" || req.Visitor == "" {
			writeProblem(w, probMissingField)
			return
		}
		if addr, ok = parseVisitorAddr(req.Visitor); !ok {
			writeProblem(w, probBadField)
			return
		}
		if !verifyPow(req.Pow, req.Source, req.Title, req.Address, s.powBits, s.now) {
			writeProblem(w, probBadProof)
			return
		}
	} else if req.Pow != "" || req.Visitor != "" {
		writeProblem(w, probBadField)
		return
	}
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	title := stripMarkup(req.Title)
	if fromForm && !s.admitVisitorLocked(addr) {
		writeProblem(w, probRateLimited)
		return
	}
	if title == "" {
		writeProblem(w, probNothingLeft)
		return
	}
	addrSum := sha256.Sum256([]byte(req.Address))
	addrKey := hex.EncodeToString(addrSum[:])
	if existingID, seen := s.addresses[addrKey]; seen {
		knownItem := s.items[existingID]
		s.counters["ledger"]++
		writeJSON(w, http.StatusOK, recordedWire{
			ID:        existingID,
			State:     knownItem.State,
			Duplicate: true,
		})
		return
	}
	s.counters["item"]++
	id := fmt.Sprintf("PA-%04d", s.counters["item"])
	it := ItemRecord{
		ID:      id,
		State:   "discovered",
		Lane:    lane,
		Source:  req.Source,
		Title:   title,
		Address: req.Address,
	}
	if result != nil {
		it.Results = map[string]json.RawMessage{caller: append(json.RawMessage(nil), result...)}
	}
	s.items[id] = it
	s.addresses[addrKey] = id
	s.counters["arrival"]++
	s.entries[id] = queueEntry{
		Key:     id,
		Subject: id,
		Kind:    "item",
		Worker:  "trawl",
		Lane:    lane,
		Arrival: s.counters["arrival"],
	}
	s.counters["ledger"]++
	writeJSON(w, http.StatusCreated, recordedWire{ID: id, State: "discovered", Next: "trawl"})
}

type healthReportWire struct {
	Component string `json:"component"`
	State     string `json:"state"`
	Code      string `json:"code,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

type healthReqWire struct {
	Contract string             `json:"contract"`
	Reports  []healthReportWire `json:"reports"`
}

type healthSummaryWire struct {
	Received int `json:"received"`
	Replayed int `json:"replayed"`
	Live     int `json:"live"`
}

var validHealthStates = map[string]bool{"healthy": true, "idle": true, "degraded": true, "paused": true}

func (s *StandIn) handleHealth(w http.ResponseWriter, r *http.Request, _ string) {
	var req healthReqWire
	if _, p := readContractBody(w, r, MaxBody, &req, &req.Contract); p != nil {
		writeProblem(w, p)
		return
	}
	if len(req.Reports) == 0 {
		writeProblem(w, probMissingField)
		return
	}
	if len(req.Reports) > maxHealthBatch {
		writeProblem(w, probBadField)
		return
	}
	seen := map[string]bool{}
	for _, rep := range req.Reports {
		if rep.Component == "" || rep.State == "" {
			writeProblem(w, probMissingField)
			return
		}
		if _, ok := registryTable[rep.Component]; !ok {
			writeProblem(w, probBadField)
			return
		}
		if seen[rep.Component] {
			writeProblem(w, probDuplicate)
			return
		}
		seen[rep.Component] = true
		switch rep.Code {
		case "", CodeTimeout, CodeParse, CodeNeedsSubtool:
		default:
			writeProblem(w, probBadField)
			return
		}
		if rep.Digest != "" && !digestFormRE.MatchString(rep.Digest) {
			writeProblem(w, probBadField)
			return
		}
	}
	for _, rep := range req.Reports {
		if !validHealthStates[rep.State] {
			writeProblem(w, probUnknownHealth)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	sum := healthSummaryWire{Received: len(req.Reports)}
	for _, rep := range req.Reports {
		c := s.components[rep.Component]
		if rep.Digest != "" {
			if c.Digest != "" && c.Digest != rep.Digest {
				reg := registryTable[rep.Component]
				if reg.worker {
					var paused []ItemRecord
					for _, it := range s.items {
						if it.State == "needs_subtool" && it.PausedBy == rep.Component {
							paused = append(paused, it)
						}
					}
					sort.Slice(paused, func(i, j int) bool { return paused[i].PausedSeq < paused[j].PausedSeq })
					for _, it := range paused {
						back := it.PausedFrom
						it.State, it.Lane = back, "urgent"
						it.PausedFrom, it.PausedBy, it.PausedSeq = "", "", 0
						s.items[it.ID] = it
						s.counters["arrival"]++
						s.entries[it.ID] = queueEntry{
							Key:     it.ID,
							Subject: it.ID,
							Kind:    "item",
							Worker:  rep.Component,
							Lane:    "urgent",
							Arrival: s.counters["arrival"],
						}
						s.counters["ledger"]++
					}
					sum.Replayed += len(paused)
				}
				var building []proposalRecord
				for _, p := range s.proposals {
					if p.Status == "building" && p.Repository == reg.repo {
						building = append(building, p)
					}
				}
				sort.Slice(building, func(i, j int) bool {
					if len(building[i].ID) != len(building[j].ID) {
						return len(building[i].ID) < len(building[j].ID)
					}
					return building[i].ID < building[j].ID
				})
				for _, p := range building {
					p.Status = "live"
					s.proposals[p.ID] = p
					s.counters["ledger"]++
					sum.Live++
				}
			}
			c.Digest = rep.Digest
		}
		c.Slug, c.State, c.Code, c.At = rep.Component, rep.State, rep.Code, s.now.UTC()
		s.components[rep.Component] = c
	}
	writeJSON(w, http.StatusOK, sum)
}

type editWire struct {
	Contract string `json:"contract"`
	Target   string `json:"target"`
	Change   string `json:"change"`
	Reason   string `json:"reason"`
	Pow      string `json:"pow"`
	Honeypot string `json:"honeypot"`
	Source   string `json:"source"`
}

type editReceiptWire struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (s *StandIn) handleEdits(w http.ResponseWriter, r *http.Request, _ string) {
	var req editWire
	size, p := readContractBody(w, r, MaxBody, &req, &req.Contract)
	if p != nil {
		writeProblem(w, p)
		return
	}
	for _, f := range []string{req.Target, req.Change, req.Reason, req.Pow, req.Honeypot, req.Source} {
		if !validTextField(f) {
			writeProblem(w, probBadField)
			return
		}
	}
	if req.Target == "" || req.Change == "" || req.Pow == "" || req.Source == "" {
		writeProblem(w, probMissingField)
		return
	}
	addr, ok := parseVisitorAddr(req.Source)
	if !ok {
		writeProblem(w, probBadField)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !verifyPow(req.Pow, req.Target, req.Change, req.Reason, s.powBits, s.now) {
		writeProblem(w, probBadProof)
		return
	}
	if req.Honeypot != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	owner, known := s.owners[req.Target]
	change, reason := stripMarkup(req.Change), stripMarkup(req.Reason)
	var verdict *problem
	switch {
	case size > maxEditBytes:
		verdict = probTooLarge
	case !known:
		verdict = probUnknownTarget
	case change == "":
		verdict = probNothingLeft
	}
	if p := s.checkStoreLocked(); p != nil {
		writeProblem(w, p)
		return
	}
	if !s.admitVisitorLocked(addr) {
		writeProblem(w, probRateLimited)
		return
	}
	if verdict != nil {
		writeProblem(w, verdict)
		return
	}
	s.counters["proposal"]++
	id := fmt.Sprintf("EP-%04d", s.counters["proposal"])
	s.proposals[id] = proposalRecord{
		ID:         id,
		Target:     req.Target,
		Repository: owner.Repository,
		File:       owner.File,
		EditKind:   owner.Kind,
		Change:     change,
		Reason:     reason,
		Status:     "pending_approval",
		Received:   s.now.UTC(),
	}
	s.counters["arrival"]++
	s.entries[id] = queueEntry{
		Key:     id,
		Subject: id,
		Kind:    "proposal",
		Worker:  "countersign",
		Lane:    "normal",
		Arrival: s.counters["arrival"],
	}
	s.counters["ledger"]++
	writeJSON(w, http.StatusAccepted, editReceiptWire{ID: id, Status: "pending_approval"})
}

func (s *StandIn) handleFeed(w http.ResponseWriter, r *http.Request, _ string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, probUnexpected)
		return
	}

	s.mu.Lock()
	if p := s.checkStoreLocked(); p != nil {
		s.mu.Unlock()
		writeProblem(w, p)
		return
	}
	if s.condSubscriberCap || len(s.subs) >= 8 {
		s.mu.Unlock()
		writeProblem(w, probBusy)
		return
	}
	var urgent, normal, backfill, paused int
	for _, e := range s.entries {
		if !entryWaiting(e, s.now) {
			continue
		}
		switch e.Lane {
		case "urgent":
			urgent++
		case "normal":
			normal++
		case "backfill":
			backfill++
		}
	}
	for _, it := range s.items {
		if it.State == "needs_subtool" {
			paused++
		}
	}
	comps := make([]componentRecord, 0, len(s.components))
	for _, c := range s.components {
		comps = append(comps, c)
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Slug < comps[j].Slug })

	ch := make(chan []byte, 64)
	ch <- s.sseFrameLocked("depths", struct {
		At           string `json:"at"`
		Urgent       int    `json:"urgent"`
		Normal       int    `json:"normal"`
		Backfill     int    `json:"backfill"`
		NeedsSubtool int    `json:"needs_subtool"`
	}{
		At:           s.now.UTC().Format(time.RFC3339),
		Urgent:       urgent,
		Normal:       normal,
		Backfill:     backfill,
		NeedsSubtool: paused,
	})
	for _, c := range comps {
		ch <- s.sseFrameLocked("health", struct {
			At        string `json:"at"`
			Component string `json:"component"`
			State     string `json:"state"`
			Code      string `json:"code,omitempty"`
		}{
			At:        c.At.UTC().Format(time.RFC3339),
			Component: c.Slug,
			State:     c.State,
			Code:      c.Code,
		})
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for len(ch) > 0 {
		frame := <-ch
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	flusher.Flush()
	<-r.Context().Done()
}

func (s *StandIn) sseFrameLocked(event string, data any) []byte {
	s.nextIDLocked()
	body, _ := json.Marshal(data)
	out := make([]byte, 0, len(body)+48)
	out = append(out, "id: "...)
	out = strconv.AppendUint(out, s.nextEventID, 10)
	out = append(out, "\nevent: "...)
	out = append(out, event...)
	out = append(out, "\ndata: "...)
	out = append(out, body...)
	out = append(out, "\n\n"...)
	return out
}

func (s *StandIn) nextIDLocked() {
	s.nextEventID++
}

func stripMarkup(s string) string {
	for {
		next := stripMarkupOnce(s)
		if next == s {
			return strings.TrimSpace(next)
		}
		s = next
	}
}

func stripMarkupOnce(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '<' {
			out.WriteByte(c)
			i++
			continue
		}
		rest := s[i:]
		if strings.HasPrefix(rest, "<!--") {
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return out.String()
			}
			i += 4 + end + 3
			continue
		}
		if len(rest) < 2 || !(isASCIIAlpha(rest[1]) || rest[1] == '/' || rest[1] == '!' || rest[1] == '?') {
			out.WriteByte(c)
			i++
			continue
		}
		end := findTagClose(rest)
		if end < 0 {
			return out.String()
		}
		name := extractTagName(rest)
		i += end + 1
		if rest[1] != '/' && (name == "script" || name == "style") {
			skip := findRawClose(s[i:], name)
			if skip < 0 {
				return out.String()
			}
			i += skip
		}
	}
	return out.String()
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func findTagClose(s string) int {
	var quote byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

func extractTagName(s string) string {
	i := 1
	if i < len(s) && s[i] == '/' {
		i++
	}
	j := i
	for j < len(s) && (isASCIIAlpha(s[j]) || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	return strings.ToLower(s[i:j])
}

func findRawClose(s, name string) int {
	lower := strings.ToLower(s)
	want := "</" + name
	from := 0
	for {
		at := strings.Index(lower[from:], want)
		if at < 0 {
			return -1
		}
		at += from
		after := at + len(want)
		if after < len(s) && (isASCIIAlpha(s[after]) || (s[after] >= '0' && s[after] <= '9')) {
			from = after
			continue
		}
		end := strings.IndexByte(s[after:], '>')
		if end < 0 {
			return -1
		}
		return after + end + 1
	}
}

func writeProblem(w http.ResponseWriter, p *problem) {
	writeJSON(w, p.Status, struct {
		Error  string `json:"error"`
		Cause  string `json:"cause"`
		Reason string `json:"reason"`
	}{
		Error:  p.Code,
		Cause:  p.Cause,
		Reason: p.Reason,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(struct {
			Error  string `json:"error"`
			Cause  string `json:"cause"`
			Reason string `json:"reason"`
		}{
			Error:  probUnexpected.Code,
			Cause:  probUnexpected.Cause,
			Reason: probUnexpected.Reason,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// ComponentCount returns the number of component health records stored in StandIn.
func (s *StandIn) ComponentCount(_ context.Context) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.components)
}
