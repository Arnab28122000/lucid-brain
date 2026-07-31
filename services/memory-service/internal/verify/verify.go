// Package verify screens extraction candidates before they are allowed to
// become memories.
//
// This is the cheapest quality lever in the system. An extractor running at
// temperature 0 still invents a price, still writes "he approved it" with no
// antecedent, still labels a passing remark a decision. Every one of those
// becomes a durable, citable, supersession-triggering record — a bad memory does
// not merely fail to help, it evicts a good one from the same topic. So the
// screen is deterministic, runs on every candidate, and rejects on doubt.
//
// Eight checks run in a fixed order, cheapest first.
package verify

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/cortex-ai/cortex/services/memory-service/internal/extract"
	"github.com/cortex-ai/cortex/services/memory-service/internal/memory"
)

// Check names, in evaluation order. They are exported because the pipeline emits
// per-check rejection counters — which check is firing tells you whether to fix
// the prompt, the chunker, or the model.
const (
	CheckShape         = "shape"
	CheckTopic         = "topic"
	CheckGrounding     = "grounding"
	CheckSelfContained = "self_contained"
	CheckKindAgreement = "kind_agreement"
	CheckTemporal      = "temporal"
	CheckAttributes    = "attribute_fidelity"
	CheckRedundancy    = "redundancy"
)

// AllChecks is the fixed order checks run in.
var AllChecks = []string{
	CheckShape, CheckTopic, CheckGrounding, CheckSelfContained,
	CheckKindAgreement, CheckTemporal, CheckAttributes, CheckRedundancy,
}

// Result is one check's outcome.
type Result struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

// Verdict is the full screen for one candidate.
type Verdict struct {
	Passed bool     `json:"passed"`
	Checks []Result `json:"checks"`
	// Penalty is subtracted from confidence for non-fatal weaknesses, so a
	// candidate that squeaks through does not present itself as certain.
	Penalty float64 `json:"penalty"`
}

// FailedCheck returns the name of the first check that failed, or "".
func (v Verdict) FailedCheck() string {
	for _, c := range v.Checks {
		if !c.Passed {
			return c.Name
		}
	}
	return ""
}

// Reason returns a human-readable rejection reason for logs and the admin
// console's extraction inspector.
func (v Verdict) Reason() string {
	for _, c := range v.Checks {
		if !c.Passed {
			return c.Name + ": " + c.Reason
		}
	}
	return ""
}

// Verifier screens candidates against their source episode.
type Verifier struct {
	// MinConfidence rejects candidates the extractor itself was unsure of.
	MinConfidence float64
	// MaxClockSkew tolerates sources whose timestamps run slightly ahead.
	MaxClockSkew time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// New returns a verifier with production thresholds.
func New() *Verifier {
	return &Verifier{
		MinConfidence: 0.35,
		MaxClockSkew:  5 * time.Minute,
		Now:           func() time.Time { return time.Now().UTC() },
	}
}

const (
	minStatementRunes = 12
	maxStatementRunes = 400
	maxTopicTokens    = 8
)

// VerifyAll screens a batch, returning the survivors and the rejected
// candidates paired with why. Batch-level state (the redundancy check) means
// candidates cannot be verified fully in isolation.
func (v *Verifier) VerifyAll(cands []extract.Candidate, ep memory.Episode) (passed []extract.Candidate, rejected []Rejection) {
	accepted := make([]extract.Candidate, 0, len(cands))
	for _, c := range cands {
		verdict := v.Verify(c, ep, accepted)
		if !verdict.Passed {
			rejected = append(rejected, Rejection{Candidate: c, Verdict: verdict})
			continue
		}
		c.Confidence = clamp01(c.Confidence - verdict.Penalty)
		accepted = append(accepted, c)
	}
	return accepted, rejected
}

// Rejection pairs a discarded candidate with its verdict. Rejections are
// persisted, not dropped: the rejection stream is the training signal for prompt
// changes and the evidence for "why is this not in the answer?" support tickets.
type Rejection struct {
	Candidate extract.Candidate
	Verdict   Verdict
}

// Verify runs all eight checks. prior holds candidates already accepted from the
// same episode, for the redundancy check.
func (v *Verifier) Verify(c extract.Candidate, ep memory.Episode, prior []extract.Candidate) Verdict {
	var verdict Verdict
	add := func(r Result, penalty float64) {
		verdict.Checks = append(verdict.Checks, r)
		if r.Passed {
			verdict.Penalty += penalty
		}
	}

	add(v.checkShape(c), 0)
	add(v.checkTopic(c), 0)

	grounding, groundingPenalty := v.checkGrounding(c, ep)
	add(grounding, groundingPenalty)

	add(v.checkSelfContained(c), 0)
	add(v.checkKindAgreement(c), 0)
	add(v.checkTemporal(c, ep), 0)

	attrs, attrPenalty := v.checkAttributes(c, ep)
	add(attrs, attrPenalty)

	add(v.checkRedundancy(c, prior), 0)

	verdict.Passed = true
	for _, r := range verdict.Checks {
		if !r.Passed {
			verdict.Passed = false
			break
		}
	}
	return verdict
}

// 1. Shape — the statement is a statement: present, sized, and asserted with
// enough confidence to be worth storing.
func (v *Verifier) checkShape(c extract.Candidate) Result {
	stmt := strings.TrimSpace(c.Statement)
	n := len([]rune(stmt))
	switch {
	case stmt == "":
		return fail(CheckShape, "empty statement")
	case n < minStatementRunes:
		return fail(CheckShape, fmt.Sprintf("statement too short (%d runes)", n))
	case n > maxStatementRunes:
		return fail(CheckShape, fmt.Sprintf("statement too long (%d runes) — likely a summary, not a memory", n))
	case !c.Kind.Valid():
		return fail(CheckShape, fmt.Sprintf("unknown kind %q", c.Kind))
	case c.Confidence < v.MinConfidence:
		return fail(CheckShape, fmt.Sprintf("confidence %.2f below %.2f", c.Confidence, v.MinConfidence))
	case strings.Count(stmt, "?") > 0 && !strings.Contains(stmt, "?\""):
		// A question is not an assertion. Extractors emit them when a thread
		// asked something nobody answered.
		return fail(CheckShape, "statement is a question, not an assertion")
	}
	return pass(CheckShape)
}

// 2. Topic — the supersession key must be specific enough to key on. A generic
// or empty topic would collapse unrelated memories into one chain and let them
// invalidate each other.
func (v *Verifier) checkTopic(c extract.Candidate) Result {
	if strings.TrimSpace(c.TopicRaw) == "" {
		return fail(CheckTopic, "missing topic")
	}
	if memory.IsGenericTopic(c.Topic) {
		return fail(CheckTopic, fmt.Sprintf("topic %q is too generic to key supersession on", c.TopicRaw))
	}
	tokens := memory.TopicTokens(c.Topic)
	if len(tokens) > maxTopicTokens {
		return fail(CheckTopic, fmt.Sprintf("topic has %d tokens — a topic that long is a summary, not a key", len(tokens)))
	}
	return pass(CheckTopic)
}

// 3. Grounding — the quote must actually be in the source. This is the check
// that catches wholesale fabrication: a model that invents a memory usually
// invents the quote supporting it too.
//
// Whitespace is normalized before comparison because models silently reflow
// text, and a memory should not be discarded over a collapsed newline.
func (v *Verifier) checkGrounding(c extract.Candidate, ep memory.Episode) (Result, float64) {
	body := normalizeWhitespace(ep.Body)
	quote := normalizeWhitespace(c.Quote)

	if quote == "" {
		// No quote is a weakness, not a lie. Fall back to checking that the
		// statement's distinctive terms appear in the source, and penalize.
		if overlap := contentOverlap(c.Statement, ep.Body); overlap < 0.4 {
			return fail(CheckGrounding, fmt.Sprintf("no quote and only %.0f%% of statement terms appear in source", overlap*100)), 0
		}
		return pass(CheckGrounding), 0.15
	}
	if strings.Contains(strings.ToLower(body), strings.ToLower(quote)) {
		return pass(CheckGrounding), 0
	}
	// Near-miss: the model paraphrased its own quote. Accept if the quote's
	// terms are overwhelmingly present, penalize, and never accept below that.
	if overlap := contentOverlap(c.Quote, ep.Body); overlap >= 0.8 {
		return pass(CheckGrounding), 0.1
	}
	return fail(CheckGrounding, "quote not found in source"), 0
}

// deictics are terms that only resolve against context the memory will not
// carry with it. A memory retrieved six months later has no "yesterday".
var deictics = []string{
	"he ", "she ", "they ", "him ", "her ", "them ", "his ", "their ",
	"it ", "its ", "this ", "that ", "these ", "those ",
	"here ", "there ", "above ", "below ", "the following ", "as mentioned ",
	"yesterday", "today", "tomorrow", "last week", "next week", "this week",
	"last month", "next month", "recently", "soon", "currently", "now ",
	"the team ", "the meeting ", "the doc ", "the ticket ", "the issue ",
}

// 4. Self-containment — the statement must be readable standing alone. This is
// the check that most often fires, and it is worth its false positives: a
// memory that says "they decided to go with the second option" is worse than no
// memory, because it retrieves confidently and explains nothing.
func (v *Verifier) checkSelfContained(c extract.Candidate) Result {
	lower := " " + strings.ToLower(strings.TrimSpace(c.Statement)) + " "
	for _, d := range deictics {
		needle := " " + d
		if strings.Contains(lower, needle) {
			// A leading capitalized possessive naming someone ("Priya's team")
			// is fine; a bare one is not. Only bare uses reach here.
			return fail(CheckSelfContained, fmt.Sprintf("unresolved reference %q — statement is not readable on its own", strings.TrimSpace(d)))
		}
	}
	if startsLowerPronoun(c.Statement) {
		return fail(CheckSelfContained, "statement opens with an unresolved reference")
	}
	return pass(CheckSelfContained)
}

var (
	decisionVerbs    = []string{"decid", "chose", "chosen", "agreed", "approved", "rejected", "settled on", "will use", "going with", "selected", "opted", "ruled out", "signed off"}
	instructionVerbs = []string{"must", "should", "always", "never", "required", "do not", "don't", "ensure", "make sure", "policy is", "rule is", "prefer"}
	taskVerbs        = []string{"will ", "to do", "todo", "action item", "owns", "assigned", "needs to", "by end of", "follow up", "picking up", "taking"}
)

// 5. Kind agreement — the claimed classification must match the statement's
// shape. Misclassification is not cosmetic: kind decides whether a memory
// supersedes, how fast it decays, and whether it appears on the decisions
// timeline. An event mislabeled a decision silently invalidates the real one.
func (v *Verifier) checkKindAgreement(c extract.Candidate) Result {
	lower := strings.ToLower(c.Statement)
	switch c.Kind {
	case memory.KindDecision:
		if !containsAny(lower, decisionVerbs) {
			return fail(CheckKindAgreement, "classified as decision but states no choice being made")
		}
	case memory.KindInstruction:
		if !containsAny(lower, instructionVerbs) {
			return fail(CheckKindAgreement, "classified as instruction but states no directive")
		}
	case memory.KindTask:
		if c.TaskStatus == "" {
			return fail(CheckKindAgreement, "task without status")
		}
		if !containsAny(lower, taskVerbs) && len(c.Subjects) == 0 {
			return fail(CheckKindAgreement, "classified as task but has neither an owner nor an action")
		}
	case memory.KindEvent, memory.KindFact:
		// Both are plain assertions; there is no lexical signature that
		// distinguishes them reliably, and guessing here would reject more good
		// memories than it catches bad ones.
	}
	if c.Kind != memory.KindTask && c.TaskStatus != "" {
		return fail(CheckKindAgreement, "task status set on a non-task")
	}
	return pass(CheckKindAgreement)
}

// 6. Temporal — bi-temporal invariants. Violations here corrupt every
// as-of query afterwards, so they are fatal rather than clamped.
func (v *Verifier) checkTemporal(c extract.Candidate, ep memory.Episode) Result {
	if c.ValidFrom.IsZero() {
		return fail(CheckTemporal, "missing valid_from")
	}
	now := v.Now()
	if c.ValidFrom.After(now.Add(v.MaxClockSkew)) {
		return fail(CheckTemporal, fmt.Sprintf("valid_from %s is in the future", c.ValidFrom.Format(time.RFC3339)))
	}
	// World time cannot precede the source that asserts it by a wide margin.
	// Some slack is legitimate — a page written today can state that a policy
	// took effect last quarter — but a decade is an extraction error.
	if ep.OccurredAt.Sub(c.ValidFrom) > 10*365*24*time.Hour {
		return fail(CheckTemporal, "valid_from implausibly precedes the source episode")
	}
	return pass(CheckTemporal)
}

var numericToken = regexp.MustCompile(`\d[\d.,:/-]*`)

// 7. Attribute fidelity — every extracted particular must appear verbatim in
// the source. This is the check the detail pass exists to be held to: a
// hallucinated price or version number is the single most damaging output this
// system can produce, because it is specific, confident, and citable.
//
// Comparison is on digits for numeric values (so "$1.02/M" matches "$1.02 / M
// tokens") and on normalized text otherwise.
func (v *Verifier) checkAttributes(c extract.Candidate, ep memory.Episode) (Result, float64) {
	if len(c.Attributes) == 0 {
		if c.Pass == extract.PassDetail {
			return fail(CheckAttributes, "detail-pass candidate carries no attributes"), 0
		}
		return pass(CheckAttributes), 0
	}

	haystack := strings.ToLower(normalizeWhitespace(ep.Body))
	haystackDigits := digitsOnly(haystack)

	for k, val := range c.Attributes {
		needle := strings.ToLower(normalizeWhitespace(val))
		if needle == "" {
			return fail(CheckAttributes, fmt.Sprintf("attribute %q is empty", k)), 0
		}
		if strings.Contains(haystack, needle) {
			continue
		}
		// Numeric values are checked digit-wise so formatting differences do
		// not reject a correct extraction — but the digits themselves must be
		// present, which is what stops an invented number.
		if nums := numericToken.FindAllString(needle, -1); len(nums) > 0 {
			allFound := true
			for _, n := range nums {
				if d := digitsOnly(n); d != "" && !strings.Contains(haystackDigits, d) {
					allFound = false
					break
				}
			}
			if allFound {
				continue
			}
			return fail(CheckAttributes, fmt.Sprintf("attribute %s=%q contains figures absent from the source", k, val)), 0
		}
		if contentOverlap(val, ep.Body) >= 0.9 {
			continue
		}
		return fail(CheckAttributes, fmt.Sprintf("attribute %s=%q not found verbatim in source", k, val)), 0
	}
	return pass(CheckAttributes), 0
}

// 8. Redundancy — a candidate must not restate one already accepted from this
// same episode. Overlapping chunks and two passes make near-duplicates routine;
// storing them would inflate every topic's supersession chain with noise.
func (v *Verifier) checkRedundancy(c extract.Candidate, prior []extract.Candidate) Result {
	for _, p := range prior {
		if p.Kind != c.Kind || p.Topic != c.Topic {
			continue
		}
		if sim := statementSimilarity(p.Statement, c.Statement); sim >= 0.7 {
			return fail(CheckRedundancy, fmt.Sprintf("restates an accepted candidate on the same topic (%.0f%% overlap)", sim*100))
		}
	}
	return pass(CheckRedundancy)
}

// --- helpers ---

func pass(name string) Result { return Result{Name: name, Passed: true} }

func fail(name, reason string) Result {
	return Result{Name: name, Passed: false, Reason: reason}
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func startsLowerPronoun(s string) bool {
	fields := strings.Fields(strings.ToLower(s))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "he", "she", "they", "it", "this", "that", "these", "those", "we", "i", "you":
		return true
	}
	return false
}

// contentOverlap and statementSimilarity live in the memory package so the
// extractor's dedupe, this screen, and the store's reinforce-versus-supersede
// decision all share one definition of "substantially the same sentence".
func contentOverlap(a, b string) float64      { return memory.ContentOverlap(a, b) }
func statementSimilarity(a, b string) float64 { return memory.StatementSimilarity(a, b) }

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}
