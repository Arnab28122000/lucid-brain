package memory

import "strings"

// Statement comparison is used in three places with three different jobs: the
// extractor collapses duplicates from overlapping chunks, the verifier rejects
// redundant candidates, and the store decides reinforcement versus
// supersession. All three want the same notion of "substantially the same
// sentence", so it lives here once — a divergence between them would show up as
// memories that dedupe during extraction but supersede each other on write.
//
// This is bag-of-content-words Jaccard, not semantics. Two sentences that mean
// the same thing in different words score low, and that is the safe direction to
// be wrong in: the cost is an extra row and a supersession edge, whereas a
// semantic matcher that wrongly merged a reversal with the thing it reversed
// would leave the stale memory current.

var similarityStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
	"by": {}, "for": {}, "from": {}, "has": {}, "in": {}, "is": {}, "it": {},
	"of": {}, "on": {}, "or": {}, "that": {}, "the": {}, "to": {}, "was": {},
	"were": {}, "will": {}, "with": {},
}

// ContentWords returns the lowercased, stopword-free, punctuation-stripped set
// of tokens in s.
func ContentWords(s string) map[string]struct{} {
	fields := strings.Fields(strings.ToLower(s))
	out := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, ".,;:!?\"'()[]{}")
		if f == "" {
			continue
		}
		if _, stop := similarityStopwords[f]; stop {
			continue
		}
		out[f] = struct{}{}
	}
	return out
}

// StatementSimilarity is the Jaccard similarity of two statements' content
// words, in [0,1].
func StatementSimilarity(a, b string) float64 {
	at, bt := ContentWords(a), ContentWords(b)
	if len(at) == 0 || len(bt) == 0 {
		return 0
	}
	var inter int
	for w := range at {
		if _, ok := bt[w]; ok {
			inter++
		}
	}
	union := len(at) + len(bt) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ContentOverlap is the fraction of a's content words that also appear in b.
// Asymmetric on purpose: grounding asks "is this statement supported by the
// source", not "are these two texts alike".
func ContentOverlap(a, b string) float64 {
	at := ContentWords(a)
	if len(at) == 0 {
		return 0
	}
	bt := ContentWords(b)
	var found int
	for w := range at {
		if _, ok := bt[w]; ok {
			found++
		}
	}
	return float64(found) / float64(len(at))
}
