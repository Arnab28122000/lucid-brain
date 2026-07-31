package memory

import (
	"sort"
	"strings"
	"unicode"
)

// Topic normalization is the hinge the whole temporal layer turns on. Two
// memories only conflict if they are keyed to the same topic, so a key that is
// too loose invalidates unrelated facts and a key that is too tight lets a stale
// Confluence page survive a Slack decision that plainly overrides it.
//
// The rules, in order: casefold, strip punctuation and possessives, drop
// stopwords, fold trivial plurals, then sort the surviving tokens. Sorting is
// what makes "pricing for the enterprise plan" and "enterprise plan pricing"
// the same key — phrasing varies between a Slack message and a wiki heading far
// more than vocabulary does.
//
// Identifiers are deliberately exempt from plural folding: `v2`, `1099-MISC`
// and `SKU-14s` must survive intact, because those are exactly the tokens that
// distinguish one topic from its neighbour.

var stopwords = map[string]struct{}{
	"a": {}, "about": {}, "all": {}, "an": {}, "and": {}, "any": {}, "are": {},
	"as": {}, "at": {}, "be": {}, "been": {}, "being": {}, "but": {}, "by": {},
	"do": {}, "does": {}, "for": {}, "from": {}, "had": {}, "has": {}, "have": {},
	"how": {}, "in": {}, "into": {}, "is": {}, "it": {}, "its": {}, "of": {},
	"on": {}, "or": {}, "our": {}, "over": {}, "should": {}, "so": {}, "some": {},
	"than": {}, "that": {}, "the": {}, "their": {}, "then": {}, "there": {},
	"these": {}, "they": {}, "this": {}, "those": {}, "to": {}, "up": {},
	"was": {}, "we": {}, "were": {}, "what": {}, "when": {}, "which": {},
	"who": {}, "will": {}, "with": {}, "would": {},
}

// genericTopics are phrasings an extractor falls back to when it has nothing
// specific to say. Keying on them would collapse unrelated memories into one
// supersession chain, so the verifier rejects them.
var genericTopics = map[string]struct{}{
	"":        {},
	"general": {},
	"info":    {},
	"misc":    {},
	"note":    {},
	"other":   {},
	"stuff":   {},
	"thing":   {},
	"topic":   {},
	"unknown": {},
	"update":  {},
	"various": {},
}

// NormalizeTopic converts a raw topic phrase into the canonical supersession key.
func NormalizeTopic(raw string) string {
	tokens := tokenize(raw)
	kept := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, stop := stopwords[t]; stop {
			continue
		}
		kept = append(kept, singularize(t))
	}
	// A phrase made entirely of stopwords ("how it is") still needs a key; fall
	// back to the raw tokens rather than returning empty and colliding with
	// every other degenerate topic.
	if len(kept) == 0 {
		kept = tokens
	}
	kept = dedupe(kept)
	sort.Strings(kept)
	return strings.Join(kept, "-")
}

// IsGenericTopic reports whether a normalized topic is too vague to key on.
func IsGenericTopic(normalized string) bool {
	if normalized == "" {
		return true
	}
	_, ok := genericTopics[normalized]
	return ok
}

// TopicTokens returns the normalized tokens of a topic, for overlap scoring in
// search and for expertise edges.
func TopicTokens(normalized string) []string {
	if normalized == "" {
		return nil
	}
	return strings.Split(normalized, "-")
}

// TopicOverlap is the Jaccard similarity of two normalized topics. Used to
// widen a topic query beyond exact key equality without reaching for embeddings.
func TopicOverlap(a, b string) float64 {
	at, bt := TopicTokens(a), TopicTokens(b)
	if len(at) == 0 || len(bt) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(at))
	for _, t := range at {
		set[t] = struct{}{}
	}
	var inter int
	seen := make(map[string]struct{}, len(bt))
	for _, t := range bt {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		if _, ok := set[t]; ok {
			inter++
		}
	}
	union := len(set) + len(seen) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokenize(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	fields := strings.FieldsFunc(s, func(r rune) bool {
		// Hyphens and dots are kept inside a token so `1099-misc`, `v1.2` and
		// `eu-central-1` survive as single identifiers. Apostrophes are kept
		// only so the possessive can be stripped below — splitting on them
		// first would turn "Priya's" into a stray "s" token.
		if r == '-' || r == '.' || r == '_' || r == '/' || r == '\'' || r == '’' {
			return false
		}
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, "-._/'’")
		f = strings.TrimSuffix(f, "'s")
		f = strings.TrimSuffix(f, "’s")
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// hasDigit marks identifiers — version strings, form numbers, SKUs, region
// codes — which must never be stemmed.
func hasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// singularize folds only the plural forms that are safe to fold without a
// dictionary. Anything ambiguous is left alone: a wrong stem silently merges two
// topics, which is worse than missing a merge.
func singularize(t string) string {
	if len(t) <= 3 || hasDigit(t) {
		return t
	}
	switch {
	case strings.HasSuffix(t, "sses"), strings.HasSuffix(t, "shes"), strings.HasSuffix(t, "ches"):
		return t[:len(t)-2] // classes -> class, batches -> batch
	case strings.HasSuffix(t, "ies") && len(t) > 4:
		return t[:len(t)-3] + "y" // policies -> policy
	case strings.HasSuffix(t, "ss"), strings.HasSuffix(t, "us"), strings.HasSuffix(t, "is"):
		return t // access, status, analysis
	case strings.HasSuffix(t, "s"):
		return t[:len(t)-1]
	}
	return t
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, t := range in {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func normalizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '@' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
