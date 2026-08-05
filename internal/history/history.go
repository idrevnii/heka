// Package history holds a bounded in-memory record of recent requests and
// warning/error-level log events, for the dashboard. Nothing here is
// persisted to disk; a restart starts empty, consistent with the rest of
// heka's stateless design.
package history

import (
	"sort"
	"sync"
	"time"
)

// Record describes one proxied request. Bodies are only populated when
// capture was enabled for the route and the response wasn't streaming.
type Record struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Route    string    `json:"route"` // provider or sidecar name (Handler.Name)
	Kind     string    `json:"kind"`  // "provider" | "sidecar"
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Query    string    `json:"query,omitempty"`
	Upstream string    `json:"upstream,omitempty"` // resolved target, key redacted
	Status   int       `json:"status"`
	Attempts int       `json:"attempts"`
	Duration int64     `json:"duration_ms"`
	Key      string    `json:"key,omitempty"` // already masked by the caller
	Verdict  string    `json:"verdict,omitempty"`
	Err      string    `json:"error,omitempty"`

	// Token accounting as reported by the provider, zero when it reported
	// none (a search API, an error response, a body heka couldn't read).
	// InputTokens includes CacheReadTokens.
	InputTokens      uint64 `json:"input_tokens,omitempty"`
	CacheReadTokens  uint64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens uint64 `json:"cache_write_tokens,omitempty"`
	OutputTokens     uint64 `json:"output_tokens,omitempty"`

	Streaming bool   `json:"streaming"`
	ReqCT     string `json:"req_content_type,omitempty"`
	RespCT    string `json:"resp_content_type,omitempty"`
	RespEnc   string `json:"resp_content_encoding,omitempty"`

	// Bodies are omitted from list views (see List) and may be evicted by
	// the byte budget even when present at insert time; BodyEvicted then
	// reports true. Never mutated in place once stored — only ever replaced
	// wholesale, so returning them by reference from Get is safe.
	ReqBody     []byte `json:"req_body,omitempty"`
	ReqTrunc    bool   `json:"req_truncated,omitempty"`
	RespBody    []byte `json:"resp_body,omitempty"`
	RespTrunc   bool   `json:"resp_truncated,omitempty"`
	BodyEvicted bool   `json:"body_evicted,omitempty"`
	CaptureSkip string `json:"capture_skipped,omitempty"` // reason capture produced nothing, e.g. "disabled", "streaming"
}

// Event is a Warn/Error-level log line captured alongside request history.
type Event struct {
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Filter narrows List results. Zero value means "no filter".
type Filter struct {
	Limit      int
	Route      string
	MinStatus  int
	MaxStatus  int
	OnlyErrors bool
	SinceID    uint64 // only records with ID > SinceID
}

type RouteCounters struct {
	Count       uint64 `json:"count"`
	Errors      uint64 `json:"errors"`
	SumDuration int64  `json:"sum_duration_ms"`
}

type Stats struct {
	Window    time.Duration `json:"window_s"`
	Count     int           `json:"count"`
	Errors    int           `json:"errors"`
	ErrorRate float64       `json:"error_rate"`
	P50Ms     int64         `json:"p50_ms"`
	P95Ms     int64         `json:"p95_ms"`
	P99Ms     int64         `json:"p99_ms"`
}

// Store is a fixed-capacity ring buffer of Records plus a separate ring of
// Events. Every method is safe to call on a nil *Store (a no-op / zero
// value), so callers never need a nil check.
type Store struct {
	mu       sync.Mutex
	buf      []Record // len == cap once filled; ring
	n, next  int      // filled count, write cursor
	seq      uint64
	bytes    int64
	maxBytes int64

	errBuf        []Event
	errN, errNext int

	started  time.Time
	total    uint64
	byStatus map[int]uint64
	byRoute  map[string]*RouteCounters
}

// New creates a Store with the given ring capacities and a body-bytes budget
// (across all captured bodies combined) that bounds memory regardless of
// per-record capture size.
func New(size, errSize int, maxBytes int64) *Store {
	if size < 1 {
		size = 1
	}
	if errSize < 1 {
		errSize = 1
	}
	return &Store{
		buf:      make([]Record, size),
		errBuf:   make([]Event, errSize),
		maxBytes: maxBytes,
		started:  time.Now(),
		byStatus: map[int]uint64{},
		byRoute:  map[string]*RouteCounters{},
	}
}

// Resize reallocates both rings, copying over the most recent
// min(old, new) records/events rather than discarding history outright.
func (s *Store) Resize(size, errSize int, maxBytes int64) {
	if s == nil {
		return
	}
	if size < 1 {
		size = 1
	}
	if errSize < 1 {
		errSize = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.snapshotRecordsLocked()
	newBuf := make([]Record, size)
	start := 0
	if len(old) > size {
		start = len(old) - size
	}
	kept := old[start:]
	copy(newBuf, kept)
	s.buf = newBuf
	s.n = len(kept)
	s.next = s.n % size
	s.bytes = 0
	for i := range kept {
		s.bytes += bodyLen(&kept[i])
	}
	s.maxBytes = maxBytes

	oldEvents := s.snapshotEventsLocked()
	newErr := make([]Event, errSize)
	start = 0
	if len(oldEvents) > errSize {
		start = len(oldEvents) - errSize
	}
	keptE := oldEvents[start:]
	copy(newErr, keptE)
	s.errBuf = newErr
	s.errN = len(keptE)
	s.errNext = s.errN % errSize

	s.evictLocked()
}

func bodyLen(r *Record) int64 {
	return int64(len(r.ReqBody) + len(r.RespBody))
}

// Add stores r, assigning it an ID, and returns that ID. Oldest captured
// bodies are evicted (metadata kept) until the store is back under budget.
func (s *Store) Add(r Record) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	r.ID = s.seq
	s.bytes += bodyLen(&r)

	s.buf[s.next] = r
	s.next = (s.next + 1) % len(s.buf)
	if s.n < len(s.buf) {
		s.n++
	}

	s.total++
	s.byStatus[r.Status]++
	rc := s.byRoute[r.Route]
	if rc == nil {
		rc = &RouteCounters{}
		s.byRoute[r.Route] = rc
	}
	rc.Count++
	rc.SumDuration += r.Duration
	if r.Status >= 400 || r.Err != "" {
		rc.Errors++
	}

	s.evictLocked()
	return r.ID
}

// evictLocked drops bodies from the oldest surviving records until the
// store's captured-body budget is respected. Must be called with mu held.
func (s *Store) evictLocked() {
	if s.maxBytes <= 0 {
		return
	}
	// Walk oldest-first: the ring's logical order starts at `next` when
	// full, or index 0 when not yet wrapped.
	start := 0
	if s.n == len(s.buf) {
		start = s.next
	}
	for i := 0; i < s.n && s.bytes > s.maxBytes; i++ {
		idx := (start + i) % len(s.buf)
		rec := &s.buf[idx]
		if len(rec.ReqBody) == 0 && len(rec.RespBody) == 0 {
			continue
		}
		s.bytes -= bodyLen(rec)
		rec.ReqBody = nil
		rec.RespBody = nil
		rec.BodyEvicted = true
	}
}

// snapshotRecordsLocked returns all filled records, oldest first. Must be
// called with mu held.
func (s *Store) snapshotRecordsLocked() []Record {
	out := make([]Record, 0, s.n)
	start := 0
	if s.n == len(s.buf) {
		start = s.next
	}
	for i := 0; i < s.n; i++ {
		out = append(out, s.buf[(start+i)%len(s.buf)])
	}
	return out
}

// List returns records matching f, newest first, with bodies omitted (use
// Get for a single record's full detail).
func (s *Store) List(f Filter) []Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	all := s.snapshotRecordsLocked()
	s.mu.Unlock()

	out := make([]Record, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		r := all[i]
		if r.ID <= f.SinceID {
			continue
		}
		if f.Route != "" && r.Route != f.Route {
			continue
		}
		if f.MinStatus != 0 && r.Status < f.MinStatus {
			continue
		}
		if f.MaxStatus != 0 && r.Status > f.MaxStatus {
			continue
		}
		if f.OnlyErrors && r.Status < 400 && r.Err == "" {
			continue
		}
		r.ReqBody = nil
		r.RespBody = nil
		out = append(out, r)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out
}

// Get returns the full record (including any captured bodies) by ID.
func (s *Store) Get(id uint64) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if s.n == len(s.buf) {
		start = s.next
	}
	for i := 0; i < s.n; i++ {
		idx := (start + i) % len(s.buf)
		if s.buf[idx].ID == id {
			return s.buf[idx], true
		}
	}
	return Record{}, false
}

// AddEvent stores a Warn/Error-level log event.
func (s *Store) AddEvent(e Event) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errBuf[s.errNext] = e
	s.errNext = (s.errNext + 1) % len(s.errBuf)
	if s.errN < len(s.errBuf) {
		s.errN++
	}
}

func (s *Store) snapshotEventsLocked() []Event {
	out := make([]Event, 0, s.errN)
	start := 0
	if s.errN == len(s.errBuf) {
		start = s.errNext
	}
	for i := 0; i < s.errN; i++ {
		out = append(out, s.errBuf[(start+i)%len(s.errBuf)])
	}
	return out
}

// Errors returns the most recent events, newest first, capped at limit (0
// means no cap).
func (s *Store) Errors(limit int) []Event {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	all := s.snapshotEventsLocked()
	s.mu.Unlock()

	out := make([]Event, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Overview is the aggregate summary for the dashboard's landing view.
type Overview struct {
	Uptime      time.Duration            `json:"uptime_s"`
	Total       uint64                   `json:"total"`
	ByStatus    map[string]uint64        `json:"by_status_class"`
	ByRoute     map[string]RouteCounters `json:"by_route"`
	Stats       Stats                    `json:"window"`
	HistorySize int                      `json:"history_size"`
	HistoryUsed int                      `json:"history_used"`
	Bytes       int64                    `json:"bytes"`
	MaxBytes    int64                    `json:"max_bytes"`
}

// Overview computes the lifetime + windowed summary used by the dashboard's
// overview tab.
func (s *Store) Overview(window time.Duration) Overview {
	if s == nil {
		return Overview{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	byStatus := map[string]uint64{}
	for code, n := range s.byStatus {
		class := statusClass(code)
		byStatus[class] += n
	}
	byRoute := make(map[string]RouteCounters, len(s.byRoute))
	for name, rc := range s.byRoute {
		byRoute[name] = *rc
	}

	return Overview{
		Uptime:      time.Since(s.started),
		Total:       s.total,
		ByStatus:    byStatus,
		ByRoute:     byRoute,
		Stats:       s.statsLocked(window),
		HistorySize: len(s.buf),
		HistoryUsed: s.n,
		Bytes:       s.bytes,
		MaxBytes:    s.maxBytes,
	}
}

func statusClass(code int) string {
	switch {
	case code == 0:
		return "unknown"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// Stats computes latency percentiles and error rate over records within the
// last window (0 means "all records currently in the ring").
func (s *Store) Stats(window time.Duration) Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked(window)
}

func (s *Store) statsLocked(window time.Duration) Stats {
	all := s.snapshotRecordsLocked()
	cutoff := time.Time{}
	if window > 0 {
		cutoff = time.Now().Add(-window)
	}
	durations := make([]int64, 0, len(all))
	errs := 0
	for _, r := range all {
		if !cutoff.IsZero() && r.Time.Before(cutoff) {
			continue
		}
		durations = append(durations, r.Duration)
		if r.Status >= 400 || r.Err != "" {
			errs++
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	st := Stats{
		Window: window,
		Count:  len(durations),
		Errors: errs,
	}
	if len(durations) > 0 {
		st.ErrorRate = float64(errs) / float64(len(durations))
		st.P50Ms = percentile(durations, 0.50)
		st.P95Ms = percentile(durations, 0.95)
		st.P99Ms = percentile(durations, 0.99)
	}
	return st
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
