package memory

import "testing"

func TestNormalizeTopic(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases and sorts", "Enterprise Plan Pricing", "enterprise-plan-pricing"},
		{"word order does not matter", "pricing for the enterprise plan", "enterprise-plan-pricing"},
		{"drops stopwords", "how we do the deploy cadence", "cadence-deploy"},
		{"strips punctuation and possessives", "Priya's on-call rotation!", "on-call-priya-rotation"},
		{"keeps identifiers intact", "1099-MISC filing deadline", "1099-misc-deadline-filing"},
		{"keeps version tokens", "vLLM v0.6.3 rollout", "rollout-v0.6.3-vllm"},
		{"folds simple plurals", "deploy policies", "deploy-policy"},
		{"does not fold -ss", "database access review", "access-database-review"},
		{"dedupes repeated tokens", "pricing pricing plan", "plan-pricing"},
		{"empty input", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeTopic(tc.in); got != tc.want {
				t.Errorf("NormalizeTopic(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The whole supersession model rests on differently-phrased references to the
// same subject producing the same key, so this gets its own test.
func TestNormalizeTopicAgreesAcrossPhrasings(t *testing.T) {
	phrasings := []string{
		"Enterprise plan pricing",
		"pricing of the Enterprise plan",
		"ENTERPRISE PLAN PRICING",
		"enterprise-plan pricing",
	}
	want := NormalizeTopic(phrasings[0])
	for _, p := range phrasings[1:] {
		if got := NormalizeTopic(p); got != want {
			t.Errorf("NormalizeTopic(%q) = %q, want %q — supersession would miss this pair", p, got, want)
		}
	}
}

func TestNormalizeTopicKeepsDistinctTopicsDistinct(t *testing.T) {
	pairs := [][2]string{
		{"eu-central-1 region latency", "us-east-1 region latency"},
		{"qdrant binary quantization", "qdrant scalar quantization"},
		{"whisper v2 accuracy", "whisper v3 accuracy"},
	}
	for _, p := range pairs {
		if a, b := NormalizeTopic(p[0]), NormalizeTopic(p[1]); a == b {
			t.Errorf("%q and %q both normalize to %q — one would silently invalidate the other", p[0], p[1], a)
		}
	}
}

func TestIsGenericTopic(t *testing.T) {
	for _, raw := range []string{"", "general", "misc", "update", "various"} {
		if !IsGenericTopic(NormalizeTopic(raw)) {
			t.Errorf("IsGenericTopic(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{"deploy cadence", "1099-MISC"} {
		if IsGenericTopic(NormalizeTopic(raw)) {
			t.Errorf("IsGenericTopic(%q) = true, want false", raw)
		}
	}
}

func TestTopicOverlap(t *testing.T) {
	a := NormalizeTopic("qdrant binary quantization")
	b := NormalizeTopic("qdrant quantization sizing")
	if got := TopicOverlap(a, b); got <= 0 || got >= 1 {
		t.Errorf("TopicOverlap(%q,%q) = %v, want a partial overlap", a, b, got)
	}
	if got := TopicOverlap(a, a); got != 1 {
		t.Errorf("TopicOverlap of identical topics = %v, want 1", got)
	}
	if got := TopicOverlap(a, NormalizeTopic("slack rate limits")); got != 0 {
		t.Errorf("TopicOverlap of unrelated topics = %v, want 0", got)
	}
}

func TestParseKind(t *testing.T) {
	cases := map[string]Kind{
		"fact": KindFact, "Facts": KindFact,
		"decision": KindDecision, "DECISIONS": KindDecision,
		"action item": KindTask, "todo": KindTask,
		"policy": KindInstruction, "preference": KindInstruction,
		"incident": KindEvent,
	}
	for in, want := range cases {
		got, err := ParseKind(in)
		if err != nil {
			t.Errorf("ParseKind(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseKind(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseKind("vibe"); err == nil {
		t.Error("ParseKind(\"vibe\") = nil error, want an error so the candidate is dropped rather than defaulted")
	}
}

func TestKindSupersedes(t *testing.T) {
	if KindEvent.Supersedes() {
		t.Error("events must not supersede — two things that happened do not contradict each other")
	}
	for _, k := range []Kind{KindFact, KindDecision, KindInstruction, KindTask} {
		if !k.Supersedes() {
			t.Errorf("%s must supersede: it describes current state", k)
		}
	}
}
