package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/api"
	"github.com/cortex-ai/cortex/services/memory-service/internal/extract"
	"github.com/cortex-ai/cortex/services/memory-service/internal/llm"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
	"github.com/cortex-ai/cortex/services/memory-service/internal/pipeline"
	"github.com/cortex-ai/cortex/services/memory-service/internal/store"
	"github.com/cortex-ai/cortex/services/memory-service/migrations"
)

const broadJSON = `{"memories":[
 {"kind":"decision","topic":"Enterprise plan pricing","statement":"Priya and Marco decided the Enterprise plan moves to $40 per seat per month from 2026-04-01.","subjects":["Priya"],"confidence":0.92,"quote":"decided to move the Enterprise plan to $40 per seat per month starting 2026-04-01"}
]}`

const detailJSON = `{"details":[
 {"kind":"decision","topic":"Enterprise plan pricing","statement":"The Enterprise plan price becomes $40 per seat per month on 2026-04-01.","attributes":{"price":"$40 per seat per month"},"confidence":0.95,"quote":"$40 per seat per month starting 2026-04-01"}
]}`

const body = `Priya: We have decided to move the Enterprise plan to $40 per seat per month starting 2026-04-01. Marco agreed.`

func testServer(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	dsn := os.Getenv("MEMORY_TEST_DSN")
	if dsn == "" {
		t.Skip("MEMORY_TEST_DSN not set; skipping API integration tests")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if strings.Contains(m.Name, "age") {
			continue
		}
		if _, err := st.Pool().Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply migration %s: %v", m.Name, err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := pipeline.New(st, extract.New(&llm.Fake{Responses: []string{broadJSON, detailJSON}}), log)
	srv := &api.Server{Store: st, Pipeline: pipe, Log: log}
	return srv.Routes(), st, fmt.Sprintf("a-%d", time.Now().UnixNano())
}

func do(t *testing.T, h http.Handler, method, path string, headers map[string]string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ingest(t *testing.T, h http.Handler, tenant string, acl []string) {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/episodes", map[string]string{
		api.HeaderTenant: tenant,
		api.HeaderGroups: strings.Join(acl, ","),
	}, api.IngestRequest{
		DocumentID:  "doc-1",
		SourceType:  "slack",
		Permalink:   "https://slack.example/p1",
		Body:        body,
		Author:      memory.Person{ExternalID: "U-priya", DisplayName: "Priya"},
		ACLGroupIDs: acl,
		OccurredAt:  time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIngestAndSearchRoundTrip(t *testing.T) {
	h, _, tenant := testServer(t)
	acl := []string{"g-pricing"}
	ingest(t, h, tenant, acl)

	rec := do(t, h, http.MethodPost, "/v1/memories/search", map[string]string{
		api.HeaderTenant: tenant,
		api.HeaderGroups: "g-pricing",
	}, api.SearchRequest{Query: "enterprise plan pricing"})
	if rec.Code != http.StatusOK {
		t.Fatalf("search returned %d: %s", rec.Code, rec.Body.String())
	}

	var resp api.SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Memories) == 0 {
		t.Fatal("search returned no memories")
	}
	if len(resp.Memories[0].Sources) == 0 {
		t.Error("memory has no citations; a memory-backed answer would be unciteable")
	}
}

func TestSearchIsFilteredByCallerGroups(t *testing.T) {
	h, _, tenant := testServer(t)
	ingest(t, h, tenant, []string{"g-pricing"})

	rec := do(t, h, http.MethodPost, "/v1/memories/search", map[string]string{
		api.HeaderTenant: tenant,
		api.HeaderGroups: "g-support",
	}, api.SearchRequest{Query: "enterprise plan pricing"})
	if rec.Code != http.StatusOK {
		t.Fatalf("search returned %d: %s", rec.Code, rec.Body.String())
	}

	var resp api.SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("an unauthorized caller saw %d memories", len(resp.Memories))
	}
}

// Fetching by ID bypasses the ACL-filtered search path, so the recheck happens
// in the handler — and a denial must be indistinguishable from absence.
func TestGetMemoryHidesExistenceFromUnauthorizedCallers(t *testing.T) {
	h, st, tenant := testServer(t)
	acl := []string{"g-pricing"}
	ingest(t, h, tenant, acl)

	mems, err := st.Search(context.Background(), store.SearchFilter{TenantID: tenant, ACLGroupIDs: acl, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) == 0 {
		t.Fatal("no memory to fetch")
	}
	id := mems[0].ID

	ok := do(t, h, http.MethodGet, "/v1/memories/"+id, map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("authorized fetch returned %d: %s", ok.Code, ok.Body.String())
	}

	denied := do(t, h, http.MethodGet, "/v1/memories/"+id, map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-support",
	}, nil)
	if denied.Code != http.StatusNotFound {
		t.Errorf("unauthorized fetch returned %d, want 404 — a 403 confirms the memory exists", denied.Code)
	}
}

func TestRequestsWithoutIdentityAreRejected(t *testing.T) {
	h, _, tenant := testServer(t)

	noTenant := do(t, h, http.MethodPost, "/v1/memories/search", map[string]string{
		api.HeaderGroups: "g-pricing",
	}, api.SearchRequest{Query: "anything"})
	if noTenant.Code != http.StatusBadRequest {
		t.Errorf("missing tenant returned %d, want 400", noTenant.Code)
	}

	noGroups := do(t, h, http.MethodPost, "/v1/memories/search", map[string]string{
		api.HeaderTenant: tenant,
	}, api.SearchRequest{Query: "anything"})
	if noGroups.Code != http.StatusBadRequest {
		t.Errorf("missing groups returned %d, want 400 — an empty result set would hide the misconfiguration", noGroups.Code)
	}
}

func TestIngestRequiresACLs(t *testing.T) {
	h, _, tenant := testServer(t)

	rec := do(t, h, http.MethodPost, "/v1/episodes", map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, api.IngestRequest{
		DocumentID: "doc-2",
		SourceType: "slack",
		Body:       body,
		OccurredAt: time.Now().UTC(),
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ingest without ACLs returned %d, want 400 — permission parity is not defaultable", rec.Code)
	}
}

func TestTimelineEndpoint(t *testing.T) {
	h, _, tenant := testServer(t)
	ingest(t, h, tenant, []string{"g-pricing"})

	rec := do(t, h, http.MethodGet, "/v1/decisions/timeline?limit=10", map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline returned %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Entries []store.TimelineEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("timeline has %d entries, want 1", len(resp.Entries))
	}
	if !resp.Entries[0].Current {
		t.Error("the only decision should be current")
	}
}

// /v1/memories/history and /v1/memories/{id} share a prefix, so this also
// pins the routing precedence between the literal segment and the wildcard.
func TestHistoryEndpoint(t *testing.T) {
	h, _, tenant := testServer(t)
	ingest(t, h, tenant, []string{"g-pricing"})

	rec := do(t, h, http.MethodGet, "/v1/memories/history?topic=Enterprise+plan+pricing&kind=decision", map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history returned %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Topic    string          `json:"topic"`
		Memories []memory.Memory `json:"memories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Topic != "enterprise-plan-pricing" {
		t.Errorf("topic = %q, want the normalized key", resp.Topic)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("history has %d entries, want 1", len(resp.Memories))
	}

	badKind := do(t, h, http.MethodGet, "/v1/memories/history?topic=x&kind=vibes", map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, nil)
	if badKind.Code != http.StatusBadRequest {
		t.Errorf("unknown kind returned %d, want 400", badKind.Code)
	}

	noTopic := do(t, h, http.MethodGet, "/v1/memories/history", map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, nil)
	if noTopic.Code != http.StatusBadRequest {
		t.Errorf("missing topic returned %d, want 400", noTopic.Code)
	}
}

func TestExpertsEndpoint(t *testing.T) {
	h, _, tenant := testServer(t)
	ingest(t, h, tenant, []string{"g-pricing"})

	rec := do(t, h, http.MethodGet, "/v1/experts?topic=Enterprise+plan+pricing", map[string]string{
		api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing",
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("experts returned %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Experts []memory.Expertise `json:"experts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Experts) == 0 {
		t.Fatal("no experts returned for a topic with an authored memory")
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	h, _, tenant := testServer(t)
	headers := map[string]string{api.HeaderTenant: tenant, api.HeaderGroups: "g-pricing"}

	badKind := do(t, h, http.MethodPost, "/v1/memories/search", headers, api.SearchRequest{Kinds: []string{"vibes"}})
	if badKind.Code != http.StatusBadRequest {
		t.Errorf("unknown kind returned %d, want 400", badKind.Code)
	}

	badTime := do(t, h, http.MethodPost, "/v1/memories/search", headers, map[string]any{"as_of": "last tuesday"})
	if badTime.Code != http.StatusBadRequest {
		t.Errorf("unparseable as_of returned %d, want 400", badTime.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/memories/search", strings.NewReader("{"))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON returned %d, want 400", rec.Code)
	}
}

func TestHealthEndpoints(t *testing.T) {
	h, _, _ := testServer(t)

	if rec := do(t, h, http.MethodGet, "/healthz", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("healthz returned %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/readyz", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("readyz returned %d", rec.Code)
	}
}
