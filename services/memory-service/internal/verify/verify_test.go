package verify

import (
	"strings"
	"testing"
	"time"

	"github.com/cortex-ai/cortex/services/memory-service/internal/extract"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

const sourceBody = `Priya: We went back and forth on this, but we've decided to move the Enterprise plan to $40 per seat per month starting 2026-04-01.
Marco: Agreed. That replaces the $32 we published on the pricing page in January.
Priya: I'll update the pricing page by Friday. Also note the EU tenants stay on eu-central-1 for now.`

func testEpisode() memory.Episode {
	return memory.Episode{
		ID:          "acme:hash",
		TenantID:    "acme",
		SourceType:  "slack",
		Body:        sourceBody,
		Author:      memory.Person{ExternalID: "U1", DisplayName: "Priya"},
		OccurredAt:  time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC),
		IngestedAt:  time.Date(2026, 3, 12, 10, 5, 0, 0, time.UTC),
		ACLGroupIDs: []string{"g-pricing"},
	}
}

func goodCandidate() extract.Candidate {
	return extract.Candidate{
		Kind:       memory.KindDecision,
		TopicRaw:   "Enterprise plan pricing",
		Topic:      memory.NormalizeTopic("Enterprise plan pricing"),
		Statement:  "Priya and Marco decided the Enterprise plan moves to $40 per seat per month from 2026-04-01.",
		Confidence: 0.9,
		Quote:      "we've decided to move the Enterprise plan to $40 per seat per month starting 2026-04-01",
		Pass:       extract.PassBroad,
		ValidFrom:  time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC),
	}
}

func newTestVerifier() *Verifier {
	v := New()
	// Freeze time so temporal checks are deterministic.
	v.Now = func() time.Time { return time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC) }
	return v
}

func TestVerifyAcceptsAWellFormedCandidate(t *testing.T) {
	v := newTestVerifier()
	verdict := v.Verify(goodCandidate(), testEpisode(), nil)
	if !verdict.Passed {
		t.Fatalf("well-formed candidate rejected: %s", verdict.Reason())
	}
	if len(verdict.Checks) != len(AllChecks) {
		t.Errorf("ran %d checks, want %d", len(verdict.Checks), len(AllChecks))
	}
}

func TestCheckShape(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	tests := []struct {
		name   string
		mutate func(*extract.Candidate)
	}{
		{"empty statement", func(c *extract.Candidate) { c.Statement = "" }},
		{"too short", func(c *extract.Candidate) { c.Statement = "yes it is" }},
		{"too long", func(c *extract.Candidate) { c.Statement = strings.Repeat("long ", 200) }},
		{"unknown kind", func(c *extract.Candidate) { c.Kind = "vibe" }},
		{"low confidence", func(c *extract.Candidate) { c.Confidence = 0.1 }},
		{"a question is not an assertion", func(c *extract.Candidate) {
			c.Statement = "Should the Enterprise plan move to $40 per seat per month?"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCandidate()
			tc.mutate(&c)
			if v.Verify(c, ep, nil).Passed {
				t.Error("candidate accepted, want rejection")
			}
		})
	}
}

func TestCheckTopicRejectsGenericKeys(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	for _, raw := range []string{"", "general", "update", "misc"} {
		c := goodCandidate()
		c.TopicRaw = raw
		c.Topic = memory.NormalizeTopic(raw)
		verdict := v.Verify(c, ep, nil)
		if verdict.Passed {
			t.Errorf("topic %q accepted — it would collapse unrelated memories into one supersession chain", raw)
		}
		if got := verdict.FailedCheck(); got != CheckTopic && raw != "" {
			t.Errorf("topic %q failed on %q, want %q", raw, got, CheckTopic)
		}
	}
}

func TestCheckGroundingRejectsAnInventedQuote(t *testing.T) {
	v := newTestVerifier()
	c := goodCandidate()
	c.Quote = "we agreed to shut down the EU region entirely next quarter"

	verdict := v.Verify(c, testEpisode(), nil)
	if verdict.Passed {
		t.Fatal("candidate with a fabricated quote accepted")
	}
	if got := verdict.FailedCheck(); got != CheckGrounding {
		t.Errorf("failed on %q, want %q", got, CheckGrounding)
	}
}

func TestCheckGroundingPenalizesAMissingQuote(t *testing.T) {
	v := newTestVerifier()
	c := goodCandidate()
	c.Quote = ""

	verdict := v.Verify(c, testEpisode(), nil)
	if !verdict.Passed {
		t.Fatalf("a well-supported statement without a quote should survive with a penalty, got: %s", verdict.Reason())
	}
	if verdict.Penalty <= 0 {
		t.Error("missing quote should carry a confidence penalty")
	}
}

func TestCheckSelfContained(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	unresolved := []string{
		"They decided to move it to $40 per seat per month.",
		"The team agreed on the new pricing yesterday.",
		"Priya decided this should go up to $40 per seat.",
	}
	for _, stmt := range unresolved {
		c := goodCandidate()
		c.Statement = stmt
		verdict := v.Verify(c, ep, nil)
		if verdict.Passed {
			t.Errorf("statement %q accepted — it is not readable six months from now", stmt)
			continue
		}
		if got := verdict.FailedCheck(); got != CheckSelfContained {
			t.Errorf("statement %q failed on %q, want %q", stmt, got, CheckSelfContained)
		}
	}
}

func TestCheckKindAgreement(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	t.Run("decision without a choice", func(t *testing.T) {
		c := goodCandidate()
		c.Statement = "The Enterprise plan costs $40 per seat per month from 2026-04-01."
		verdict := v.Verify(c, ep, nil)
		if verdict.Passed {
			t.Fatal("a bare assertion labelled a decision was accepted; it would appear on the decisions timeline")
		}
		if got := verdict.FailedCheck(); got != CheckKindAgreement {
			t.Errorf("failed on %q, want %q", got, CheckKindAgreement)
		}
	})

	t.Run("same statement is fine as a fact", func(t *testing.T) {
		c := goodCandidate()
		c.Kind = memory.KindFact
		c.Statement = "The Enterprise plan costs $40 per seat per month from 2026-04-01."
		if verdict := v.Verify(c, ep, nil); !verdict.Passed {
			t.Fatalf("rejected as a fact: %s", verdict.Reason())
		}
	})

	t.Run("task without a status", func(t *testing.T) {
		c := goodCandidate()
		c.Kind = memory.KindTask
		c.Statement = "Priya will update the pricing page by Friday 2026-03-13."
		c.TaskStatus = ""
		if v.Verify(c, ep, nil).Passed {
			t.Fatal("task without a status accepted")
		}
	})

	t.Run("task status on a non-task", func(t *testing.T) {
		c := goodCandidate()
		c.TaskStatus = memory.TaskOpen
		if v.Verify(c, ep, nil).Passed {
			t.Fatal("decision carrying a task status accepted")
		}
	})
}

func TestCheckTemporal(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	t.Run("future valid_from", func(t *testing.T) {
		c := goodCandidate()
		c.ValidFrom = v.Now().Add(48 * time.Hour)
		verdict := v.Verify(c, ep, nil)
		if verdict.Passed {
			t.Fatal("a memory valid in the future was accepted")
		}
		if got := verdict.FailedCheck(); got != CheckTemporal {
			t.Errorf("failed on %q, want %q", got, CheckTemporal)
		}
	})

	t.Run("missing valid_from", func(t *testing.T) {
		c := goodCandidate()
		c.ValidFrom = time.Time{}
		if v.Verify(c, ep, nil).Passed {
			t.Fatal("a memory with no valid_from was accepted; every as-of query would misplace it")
		}
	})

	t.Run("small clock skew is tolerated", func(t *testing.T) {
		c := goodCandidate()
		c.ValidFrom = v.Now().Add(2 * time.Minute)
		if !v.Verify(c, ep, nil).Passed {
			t.Fatal("two minutes of clock skew should not reject a memory")
		}
	})
}

func TestCheckAttributeFidelity(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	t.Run("verbatim attributes pass", func(t *testing.T) {
		c := goodCandidate()
		c.Pass = extract.PassDetail
		c.Attributes = map[string]string{"price": "$40 per seat per month", "effective": "2026-04-01"}
		if verdict := v.Verify(c, ep, nil); !verdict.Passed {
			t.Fatalf("verbatim attributes rejected: %s", verdict.Reason())
		}
	})

	t.Run("reformatted numbers still pass", func(t *testing.T) {
		c := goodCandidate()
		c.Pass = extract.PassDetail
		c.Attributes = map[string]string{"price": "$40/seat/month"}
		if verdict := v.Verify(c, ep, nil); !verdict.Passed {
			t.Fatalf("reformatted but truthful figure rejected: %s", verdict.Reason())
		}
	})

	t.Run("invented figures are rejected", func(t *testing.T) {
		c := goodCandidate()
		c.Pass = extract.PassDetail
		c.Attributes = map[string]string{"price": "$45 per seat per month"}
		verdict := v.Verify(c, ep, nil)
		if verdict.Passed {
			t.Fatal("a hallucinated price was accepted — this is the worst output the system can produce")
		}
		if got := verdict.FailedCheck(); got != CheckAttributes {
			t.Errorf("failed on %q, want %q", got, CheckAttributes)
		}
	})

	t.Run("detail candidate without attributes is rejected", func(t *testing.T) {
		c := goodCandidate()
		c.Pass = extract.PassDetail
		c.Attributes = nil
		if v.Verify(c, ep, nil).Passed {
			t.Fatal("detail-pass candidate with no specifics accepted")
		}
	})
}

func TestCheckRedundancy(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	first := goodCandidate()
	second := goodCandidate()
	second.Statement = "Priya and Marco decided the Enterprise plan moves to $40 per seat per month from 2026-04-01!"

	verdict := v.Verify(second, ep, []extract.Candidate{first})
	if verdict.Passed {
		t.Fatal("a restatement of an already-accepted candidate was accepted")
	}
	if got := verdict.FailedCheck(); got != CheckRedundancy {
		t.Errorf("failed on %q, want %q", got, CheckRedundancy)
	}
}

func TestVerifyAllKeepsDistinctCandidatesAndDropsDuplicates(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	pricing := goodCandidate()
	duplicate := goodCandidate()
	region := extract.Candidate{
		Kind:       memory.KindFact,
		TopicRaw:   "EU tenant region",
		Topic:      memory.NormalizeTopic("EU tenant region"),
		Statement:  "EU tenants remain on the eu-central-1 region as of 2026-03-12.",
		Confidence: 0.8,
		Quote:      "the EU tenants stay on eu-central-1 for now",
		Pass:       extract.PassBroad,
		ValidFrom:  ep.OccurredAt,
	}

	passed, rejected := v.VerifyAll([]extract.Candidate{pricing, duplicate, region}, ep)
	if len(passed) != 2 {
		t.Fatalf("accepted %d candidates, want 2 (pricing + region)", len(passed))
	}
	if len(rejected) != 1 {
		t.Fatalf("rejected %d candidates, want 1 (the duplicate)", len(rejected))
	}
	if got := rejected[0].Verdict.FailedCheck(); got != CheckRedundancy {
		t.Errorf("duplicate rejected on %q, want %q", got, CheckRedundancy)
	}
}

func TestPenaltyReducesStoredConfidence(t *testing.T) {
	v := newTestVerifier()
	ep := testEpisode()

	c := goodCandidate()
	c.Quote = "" // grounding penalty, no rejection

	passed, _ := v.VerifyAll([]extract.Candidate{c}, ep)
	if len(passed) != 1 {
		t.Fatalf("expected the candidate to survive, got %d", len(passed))
	}
	if passed[0].Confidence >= c.Confidence {
		t.Errorf("confidence %v was not reduced from %v despite a penalty", passed[0].Confidence, c.Confidence)
	}
}
