package extract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/llm"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

func TestChunkBodyKeepsShortBodiesWhole(t *testing.T) {
	chunks := ChunkBody("A short thread about deploy cadence.")
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if chunks[0].Text != "A short thread about deploy cadence." {
		t.Errorf("chunk text = %q", chunks[0].Text)
	}
}

func TestChunkBodySplitsOnParagraphBoundaries(t *testing.T) {
	para := strings.Repeat("This is a sentence about the pricing decision. ", 120) // ~5.6K chars
	body := para + "\n\n" + para + "\n\n" + para

	chunks := ChunkBody(body)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks for a %d-char body, want at least 2", len(chunks), len(body))
	}
	for _, c := range chunks {
		if len(c.Text) > broadChunkChars {
			t.Errorf("chunk %d is %d chars, over the %d limit", c.Index, len(c.Text), broadChunkChars)
		}
		if strings.TrimSpace(c.Text) == "" {
			t.Errorf("chunk %d is empty", c.Index)
		}
	}
}

func TestChunkBodyOverlapsSoStraddlingContentIsSeenTwice(t *testing.T) {
	body := strings.Repeat("word ", 4000) // 20K chars, no paragraph breaks
	chunks := ChunkBody(body)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2", len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Start >= chunks[i-1].End {
			t.Errorf("chunk %d starts at %d, at or after the end of chunk %d (%d) — no overlap window",
				i, chunks[i].Start, i-1, chunks[i-1].End)
		}
	}
}

func TestChunkBodyDoesNotEmitARuntFinalChunk(t *testing.T) {
	// A body just over the limit should stay one chunk rather than paying an
	// LLM call for a two-sentence tail.
	body := strings.Repeat("a", broadChunkChars+400)
	if chunks := ChunkBody(body); len(chunks) != 1 {
		t.Fatalf("got %d chunks for a body %d over the limit, want 1", len(chunks), 400)
	}
}

func testEpisode(body string) memory.Episode {
	return memory.Episode{
		ID:         "acme:hash",
		TenantID:   "acme",
		SourceType: "slack",
		Body:       body,
		Author:     memory.Person{ExternalID: "U1", DisplayName: "Priya"},
		OccurredAt: time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC),
	}
}

const broadJSON = `{"memories":[
  {"kind":"decision","topic":"Enterprise plan pricing","statement":"Priya decided the Enterprise plan moves to $40 per seat per month.","subjects":["Priya"],"confidence":0.9,"quote":"we've decided to move the Enterprise plan to $40"},
  {"kind":"nonsense","topic":"something","statement":"Unparseable kind.","confidence":0.9,"quote":"x"}
]}`

const detailJSON = `{"details":[
  {"kind":"decision","topic":"Enterprise plan pricing","statement":"The Enterprise plan price is $40 per seat per month from 2026-04-01.","attributes":{"price":"$40 per seat per month","effective":"2026-04-01"},"confidence":0.95,"quote":"$40 per seat per month starting 2026-04-01"},
  {"kind":"fact","topic":"EU tenant region","statement":"EU tenants stay on eu-central-1.","attributes":{},"confidence":0.8,"quote":"EU tenants stay on eu-central-1"}
]}`

func TestExtractMergesBothPasses(t *testing.T) {
	fake := &llm.Fake{Responses: []string{broadJSON, detailJSON}}
	ex := New(fake)

	cands, errs := ex.Extract(context.Background(), testEpisode("a short thread about pricing"))
	if len(errs) != 0 {
		t.Fatalf("unexpected extraction errors: %v", errs)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1 merged pricing candidate", len(cands))
	}

	c := cands[0]
	if c.Pass != PassMerged {
		t.Errorf("pass = %q, want %q", c.Pass, PassMerged)
	}
	if c.Attributes["price"] != "$40 per seat per month" {
		t.Errorf("detail attributes were not grafted onto the broad statement: %v", c.Attributes)
	}
	if !strings.Contains(c.Statement, "Priya decided") {
		t.Errorf("merged candidate should keep the broad pass's statement, got %q", c.Statement)
	}
	if c.Confidence != 0.95 {
		t.Errorf("confidence = %v, want the higher of the two passes (0.95)", c.Confidence)
	}
}

func TestExtractDropsUnclassifiableCandidates(t *testing.T) {
	// The broad response above carries one candidate with kind "nonsense". It
	// must be dropped rather than defaulted to fact, or it would enter the
	// supersession path and invalidate a real fact on its topic.
	fake := &llm.Fake{Responses: []string{broadJSON, `{"details":[]}`}}
	cands, _ := New(fake).Extract(context.Background(), testEpisode("thread"))

	for _, c := range cands {
		if strings.Contains(c.Statement, "Unparseable") {
			t.Fatal("candidate with an unknown kind survived extraction")
		}
	}
}

func TestExtractDropsDetailCandidatesWithNoAttributes(t *testing.T) {
	fake := &llm.Fake{Responses: []string{`{"memories":[]}`, detailJSON}}
	cands, _ := New(fake).Extract(context.Background(), testEpisode("thread"))

	for _, c := range cands {
		if strings.Contains(c.Statement, "eu-central-1") {
			t.Fatal("detail candidate with an empty attribute map survived; it is just a worse broad candidate")
		}
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
}

func TestExtractSurvivesMalformedJSON(t *testing.T) {
	fake := &llm.Fake{Responses: []string{"I'm sorry, I can't help with that.", detailJSON}}
	cands, errs := New(fake).Extract(context.Background(), testEpisode("thread"))

	if len(errs) == 0 {
		t.Fatal("a malformed pass should surface an error rather than be swallowed")
	}
	if len(cands) == 0 {
		t.Fatal("the surviving pass's candidates should still be returned")
	}
}

func TestExtractHandlesFencedJSON(t *testing.T) {
	fenced := "Here you go:\n```json\n" + broadJSON + "\n```"
	fake := &llm.Fake{Responses: []string{fenced, `{"details":[]}`}}

	cands, errs := New(fake).Extract(context.Background(), testEpisode("thread"))
	if len(errs) != 0 {
		t.Fatalf("fenced JSON should parse, got errors: %v", errs)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
}

func TestMergeKeepsDetailOnlyTopics(t *testing.T) {
	broad := []Candidate{{
		Kind: memory.KindFact, Topic: "deploy-cadence", Statement: "Deploys happen weekly.", Confidence: 0.8,
	}}
	detail := []Candidate{{
		Kind: memory.KindFact, Topic: "slack-rate-limit", Statement: "Slack allows 1 request per minute.",
		Attributes: map[string]string{"limit": "1 request/minute"}, Confidence: 0.9, Pass: PassDetail,
	}}

	merged := Merge(broad, detail)
	if len(merged) != 2 {
		t.Fatalf("got %d merged candidates, want 2 — a specific nobody summarized is still a memory", len(merged))
	}
}

func TestMergeDedupesAcrossOverlappingChunks(t *testing.T) {
	same := Candidate{
		Kind: memory.KindFact, Topic: "deploy-cadence",
		Statement: "Deploys happen weekly on Thursday.", Confidence: 0.8,
	}
	other := same
	other.ChunkIndex = 1

	merged := Merge([]Candidate{same, other}, nil)
	if len(merged) != 1 {
		t.Fatalf("got %d candidates, want 1 — the same sentence seen through the overlap window", len(merged))
	}
}

func TestRenderPromptGivesTheModelAnAbsoluteDate(t *testing.T) {
	ep := testEpisode("body text")
	prompt := renderPrompt(ep, Chunk{Text: "body text"})
	if !strings.Contains(prompt, "2026-03-12") {
		t.Error("the prompt must state the episode date so the model can resolve relative time into absolute dates")
	}
	if !strings.Contains(prompt, "Priya") {
		t.Error("the prompt must name the author so statements can be attributed")
	}
}

func TestNormalizeSubjectsResolvesToStableIdentifiers(t *testing.T) {
	ep := memory.Episode{
		Author:       memory.Person{ExternalID: "U1", DisplayName: "Priya"},
		Participants: []memory.Person{{ExternalID: "U2", DisplayName: "Marco"}},
	}
	got := normalizeSubjects([]string{"Priya", "marco", "Someone Else"}, ep)

	want := map[string]bool{"U1": true, "U2": true, "name:someone else": true}
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 subjects", got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected subject key %q", k)
		}
	}
}
