package extract

import "strings"

// Chunking for extraction is not chunking for retrieval, and conflating the two
// is a common mistake. Retrieval wants small chunks so the embedding is
// specific; extraction wants *large* chunks so the model can see the whole
// argument — the decision at the bottom of a Slack thread only makes sense with
// the debate above it. Hence ~10K characters, which comfortably holds a long
// thread or a wiki page section while staying inside a cheap context window.
const (
	broadChunkChars = 10000
	// overlap carries the tail of the previous chunk forward so a decision that
	// straddles a boundary is not cut in half.
	chunkOverlapChars = 800
	// minTailChars folds a runt final chunk back into its predecessor rather
	// than paying a whole LLM call for two sentences.
	minTailChars = 1200
)

// Chunk is a slice of episode body with the offsets needed to map a quote back
// to its position in the source.
type Chunk struct {
	Index int
	Start int
	End   int
	Text  string
}

// ChunkBody splits an episode body into overlapping extraction windows,
// preferring paragraph then sentence boundaries so a chunk never begins
// mid-clause.
func ChunkBody(body string) []Chunk {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if len(body) <= broadChunkChars {
		return []Chunk{{Index: 0, Start: 0, End: len(body), Text: body}}
	}

	var chunks []Chunk
	start := 0
	for start < len(body) {
		end := start + broadChunkChars
		if end >= len(body) {
			end = len(body)
		} else {
			end = boundaryBefore(body, start, end)
		}
		// Fold a short remainder into the current chunk instead of emitting it.
		if len(body)-end < minTailChars {
			end = len(body)
		}
		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Start: start,
			End:   end,
			Text:  strings.TrimSpace(body[start:end]),
		})
		if end >= len(body) {
			break
		}
		next := end - chunkOverlapChars
		if next <= start {
			next = end
		}
		start = next
	}
	return chunks
}

// boundaryBefore walks back from the hard limit to the nearest paragraph break,
// then to the nearest sentence end, then gives up and cuts at a space.
func boundaryBefore(s string, start, limit int) int {
	window := s[start:limit]
	minAccept := len(window) / 2 // never give back more than half the chunk

	if i := strings.LastIndex(window, "\n\n"); i > minAccept {
		return start + i + 2
	}
	for _, term := range []string{". ", ".\n", "? ", "! ", "\n"} {
		if i := strings.LastIndex(window, term); i > minAccept {
			return start + i + len(term)
		}
	}
	if i := strings.LastIndexByte(window, ' '); i > minAccept {
		return start + i + 1
	}
	return limit
}
