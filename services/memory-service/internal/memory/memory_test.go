package memory

import (
	"testing"
	"time"
)

func TestCurrentAt(t *testing.T) {
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	closed := Memory{ValidFrom: jan, ValidTo: &mar}
	open := Memory{ValidFrom: mar}

	if !closed.CurrentAt(feb) {
		t.Error("a memory valid Jan-Mar must be current in February")
	}
	if closed.CurrentAt(mar) {
		t.Error("valid_to is exclusive: the superseded memory must not be current at the instant its replacement begins")
	}
	if open.CurrentAt(feb) {
		t.Error("a memory beginning in March must not be current in February")
	}
	if !open.CurrentAt(mar) {
		t.Error("valid_from is inclusive")
	}
	if closed.IsCurrent() {
		t.Error("a memory with valid_to set is not current")
	}
	if !open.IsCurrent() {
		t.Error("a memory with no valid_to is current")
	}
}

func TestTopicKeySeparatesKinds(t *testing.T) {
	base := Memory{TenantID: "acme", Topic: "deploy-cadence"}
	fact := base
	fact.Kind = KindFact
	decision := base
	decision.Kind = KindDecision

	if fact.TopicKey() == decision.TopicKey() {
		t.Error("a decision about a topic must not share a supersession key with a fact about it")
	}
}

func TestTopicKeySeparatesTenants(t *testing.T) {
	a := Memory{TenantID: "acme", Kind: KindFact, Topic: "deploy-cadence"}
	b := Memory{TenantID: "globex", Kind: KindFact, Topic: "deploy-cadence"}
	if a.TopicKey() == b.TopicKey() {
		t.Error("tenants must not share supersession keys")
	}
}

func TestPersonKeyPrefersStableIdentifier(t *testing.T) {
	p := Person{ExternalID: "U123", DisplayName: "Priya R", Email: "priya@example.com"}
	if got := p.Key(); got != "U123" {
		t.Errorf("Key() = %q, want the external ID", got)
	}
	if got := (Person{Email: "Priya@Example.com "}).Key(); got != "email:priya@example.com" {
		t.Errorf("email fallback = %q, want a normalized email key", got)
	}
	if got := (Person{DisplayName: "Priya R"}).Key(); got != "name:priyar" {
		t.Errorf("name fallback = %q", got)
	}
}

func TestHalfLifeOrdering(t *testing.T) {
	// Decisions must outlive casual facts, or the institutional memory the
	// layer exists to hold is the first thing decay throws away.
	if KindDecision.HalfLife() <= KindFact.HalfLife() {
		t.Error("decisions must decay more slowly than facts")
	}
	if KindTask.HalfLife() >= KindFact.HalfLife() {
		t.Error("tasks must decay faster than facts")
	}
}

func TestStatementSimilarity(t *testing.T) {
	same := StatementSimilarity(
		"The enterprise plan costs $40 per seat per month.",
		"The enterprise plan costs $40 per seat per month.")
	if same != 1 {
		t.Errorf("identical statements scored %v, want 1", same)
	}

	near := StatementSimilarity(
		"The enterprise plan costs $40 per seat per month.",
		"Enterprise plan costs $40 per seat each month.")
	if near < 0.5 {
		t.Errorf("near-identical statements scored %v, want >= 0.5", near)
	}

	different := StatementSimilarity(
		"The enterprise plan costs $40 per seat per month.",
		"Slack ingestion is rate limited to one request per minute.")
	if different > 0.1 {
		t.Errorf("unrelated statements scored %v, want ~0", different)
	}
}

func TestContentOverlapIsAsymmetric(t *testing.T) {
	short := "Qdrant binary quantization enabled"
	long := "The team enabled Qdrant binary quantization last week after the RAM alert fired repeatedly in production."

	if got := ContentOverlap(short, long); got != 1 {
		t.Errorf("every content word of the short statement appears in the long one; got %v, want 1", got)
	}
	if got := ContentOverlap(long, short); got >= 1 {
		t.Errorf("overlap must be asymmetric; got %v", got)
	}
}
