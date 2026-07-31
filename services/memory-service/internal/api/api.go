// Package api serves the memory layer's read path to the query gateway and the
// decisions timeline UI, plus a small admin surface.
//
// Callers are internal services, so the contract is JSON over HTTP with the
// caller's identity and expanded groups passed in headers by the gateway, which
// has already done the SpiceDB expansion. The memory service does not re-expand
// groups — it applies them. Duplicating the expansion here would put a second
// permission implementation in the blast radius of every permission bug.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/pipeline"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
)

// Headers the query gateway sets on every call.
const (
	HeaderTenant = "X-Cortex-Tenant-Id"
	HeaderGroups = "X-Cortex-Acl-Groups" // comma-separated, already expanded
	HeaderAdmin  = "X-Cortex-Admin"      // set only by the control plane
)

// Server holds the API dependencies.
type Server struct {
	Store    *store.Store
	Pipeline *pipeline.Pipeline
	Log      *slog.Logger
	Ready    func() bool
}

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	mux.HandleFunc("POST /v1/memories/search", s.handleSearch)
	mux.HandleFunc("GET /v1/memories/{id}", s.handleGetMemory)
	mux.HandleFunc("GET /v1/memories/history", s.handleHistory)
	mux.HandleFunc("GET /v1/decisions/timeline", s.handleTimeline)
	mux.HandleFunc("GET /v1/experts", s.handleExperts)
	mux.HandleFunc("GET /v1/stats", s.handleStats)

	// Direct episode submission, for connectors that do not publish to
	// JetStream, for replaying a single document during an incident, and for
	// the eval harness.
	mux.HandleFunc("POST /v1/episodes", s.handleIngestEpisode)

	return withRecovery(withLogging(s.Log, mux))
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	if s.Ready != nil && !s.Ready() {
		writeError(w, http.StatusServiceUnavailable, "event consumer not connected")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// SearchRequest is the query gateway's memory lookup, issued in parallel with
// hybrid search so memory injection costs no extra serial latency.
type SearchRequest struct {
	Query             string   `json:"query"`
	Topic             string   `json:"topic,omitempty"`
	Kinds             []string `json:"kinds,omitempty"`
	Subjects          []string `json:"subjects,omitempty"`
	AsOf              *string  `json:"as_of,omitempty"`
	IncludeSuperseded bool     `json:"include_superseded,omitempty"`
	MinConfidence     float64  `json:"min_confidence,omitempty"`
	Limit             int      `json:"limit,omitempty"`
}

// SearchResponse carries memories with their citations.
type SearchResponse struct {
	Memories []memory.Memory `json:"memories"`
	AsOf     time.Time       `json:"as_of"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	tenant, groups, ok := identity(w, r)
	if !ok {
		return
	}

	var req SearchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	kinds, err := parseKinds(req.Kinds)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	asOf, err := parseTimePtr(req.AsOf)
	if err != nil {
		writeError(w, http.StatusBadRequest, "as_of: "+err.Error())
		return
	}

	mems, err := s.Store.Search(r.Context(), store.SearchFilter{
		TenantID:          tenant,
		Query:             req.Query,
		Topic:             req.Topic,
		Kinds:             kinds,
		Subjects:          req.Subjects,
		ACLGroupIDs:       groups,
		AsOf:              asOf,
		IncludeSuperseded: req.IncludeSuperseded,
		MinConfidence:     req.MinConfidence,
		Limit:             req.Limit,
	})
	if err != nil {
		s.fail(w, "search", err)
		return
	}

	effective := time.Now().UTC()
	if asOf != nil {
		effective = *asOf
	}
	writeJSON(w, http.StatusOK, SearchResponse{Memories: nonNilMemories(mems), AsOf: effective})
}

func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	tenant, groups, ok := identity(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	m, err := s.Store.Get(r.Context(), tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	if err != nil {
		s.fail(w, "get memory", err)
		return
	}
	// Late-binding recheck. The row was fetched by ID rather than through the
	// ACL-filtered search path, so authorization is applied here — and a denial
	// returns 404, not 403, because "this memory exists but is not for you"
	// leaks the fact that it exists.
	if !isAdmin(r) && !intersects(m.ACLGroupIDs, groups) {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	tenant, groups, ok := identity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	topic := q.Get("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}
	kind := memory.Kind(q.Get("kind"))
	if kind == "" {
		kind = memory.KindFact
	}
	if !kind.Valid() {
		writeError(w, http.StatusBadRequest, "unknown kind")
		return
	}

	mems, err := s.Store.History(r.Context(), tenant, kind, topic, groups, isAdmin(r))
	if err != nil {
		s.fail(w, "history", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"topic":    memory.NormalizeTopic(topic),
		"kind":     kind,
		"memories": nonNilMemories(mems),
	})
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	tenant, groups, ok := identity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()

	from, err := parseTimePtr(strPtr(q.Get("from")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from: "+err.Error())
		return
	}
	to, err := parseTimePtr(strPtr(q.Get("to")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to: "+err.Error())
		return
	}
	kinds, err := parseKinds(splitCSV(q.Get("kinds")))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	entries, err := s.Store.Timeline(r.Context(), store.TimelineFilter{
		TenantID:    tenant,
		Topic:       q.Get("topic"),
		Kinds:       kinds,
		From:        from,
		To:          to,
		ACLGroupIDs: groups,
		SkipACL:     isAdmin(r),
		Limit:       atoiDefault(q.Get("limit"), 50),
	})
	if err != nil {
		s.fail(w, "timeline", err)
		return
	}
	if entries == nil {
		entries = []store.TimelineEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleExperts(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := identity(w, r)
	if !ok {
		return
	}
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "topic is required")
		return
	}

	experts, err := s.Store.Experts(r.Context(), tenant, topic, atoiDefault(r.URL.Query().Get("limit"), 10))
	if err != nil {
		s.fail(w, "experts", err)
		return
	}
	if experts == nil {
		experts = []memory.Expertise{}
	}
	// Expertise is an aggregate over topics rather than a document, so it
	// carries no ACL of its own. Names of colleagues and the topics they work
	// on are directory-level information; the memories behind the ranking stay
	// ACL-filtered when the caller opens them.
	writeJSON(w, http.StatusOK, map[string]any{
		"topic":   memory.NormalizeTopic(topic),
		"experts": experts,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := identity(w, r)
	if !ok {
		return
	}
	stats, err := s.Store.Stats(r.Context(), tenant)
	if err != nil {
		s.fail(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// IngestRequest submits one episode directly.
type IngestRequest struct {
	DocumentID   string          `json:"document_id"`
	ContentHash  string          `json:"content_hash"`
	SourceType   string          `json:"source_type"`
	SourceID     string          `json:"source_id,omitempty"`
	Permalink    string          `json:"permalink,omitempty"`
	Title        string          `json:"title,omitempty"`
	Body         string          `json:"body"`
	Author       memory.Person   `json:"author"`
	Participants []memory.Person `json:"participants,omitempty"`
	ACLGroupIDs  []string        `json:"acl_group_ids"`
	OccurredAt   time.Time       `json:"occurred_at"`
}

func (s *Server) handleIngestEpisode(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := identity(w, r)
	if !ok {
		return
	}
	if s.Pipeline == nil {
		writeError(w, http.StatusNotImplemented, "pipeline not configured")
		return
	}

	var req IngestRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case strings.TrimSpace(req.Body) == "":
		writeError(w, http.StatusBadRequest, "body is required")
		return
	case req.DocumentID == "":
		writeError(w, http.StatusBadRequest, "document_id is required")
		return
	case len(req.ACLGroupIDs) == 0:
		// Permission parity is not defaultable. An episode with no ACLs would
		// produce memories nobody can see, or — worse, if the filter were ever
		// relaxed — memories everybody can see.
		writeError(w, http.StatusBadRequest, "acl_group_ids is required")
		return
	}

	occurred := req.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	hash := req.ContentHash
	if hash == "" {
		hash = contentHash(req.Body)
	}

	ep := memory.Episode{
		ID:           tenant + ":" + hash,
		TenantID:     tenant,
		DocumentID:   req.DocumentID,
		ContentHash:  hash,
		SourceType:   req.SourceType,
		SourceID:     req.SourceID,
		Permalink:    req.Permalink,
		Title:        req.Title,
		Body:         req.Body,
		Author:       req.Author,
		Participants: req.Participants,
		ACLGroupIDs:  req.ACLGroupIDs,
		OccurredAt:   occurred.UTC(),
		IngestedAt:   time.Now().UTC(),
	}

	report, err := s.Pipeline.Process(r.Context(), ep)
	if err != nil {
		s.fail(w, "ingest episode", err)
		return
	}
	writeJSON(w, http.StatusAccepted, report)
}

// --- middleware and helpers ---

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if log != nil {
			log.Debug("request",
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}
	})
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// identity extracts the tenant and expanded ACL groups the gateway resolved.
// A request without a tenant is rejected outright rather than defaulted —
// there is no sensible default tenant in a multi-tenant store.
func identity(w http.ResponseWriter, r *http.Request) (tenant string, groups []string, ok bool) {
	tenant = strings.TrimSpace(r.Header.Get(HeaderTenant))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "missing "+HeaderTenant)
		return "", nil, false
	}
	groups = splitCSV(r.Header.Get(HeaderGroups))
	if len(groups) == 0 && !isAdmin(r) {
		// Empty groups would match nothing anyway, but failing loudly turns a
		// silently empty result set into an obvious misconfiguration.
		writeError(w, http.StatusBadRequest, "missing "+HeaderGroups)
		return "", nil, false
	}
	return tenant, groups, true
}

func isAdmin(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get(HeaderAdmin), "true")
}

func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	if s.Log != nil {
		s.Log.Error("request failed", "op", op, "error", err)
	}
	// The error text is not returned: it can contain SQL fragments and column
	// names, and the caller can do nothing with either.
	writeError(w, http.StatusInternalServerError, "internal error")
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func parseKinds(in []string) ([]memory.Kind, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]memory.Kind, 0, len(in))
	for _, raw := range in {
		k, err := memory.ParseKind(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

func parseTimePtr(s *string) (*time.Time, error) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*s))
	if err != nil {
		return nil, errors.New("expected RFC3339 timestamp")
	}
	t = t.UTC()
	return &t, nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func intersects(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

func nonNilMemories(in []memory.Memory) []memory.Memory {
	if in == nil {
		return []memory.Memory{}
	}
	return in
}

// contentHash derives the idempotency key for episodes submitted without one.
// It must match what the ingestion plane computes — SHA-256 over the normalized
// body — or the same content arriving by both paths would extract twice.
func contentHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:])
}
