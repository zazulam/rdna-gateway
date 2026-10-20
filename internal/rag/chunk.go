// Package rag is retrieval for agents: chunk text, embed it with a llama-server
// embedding model, store it in Qdrant, and search it with an optional reranker.
package rag

import "strings"

// Chunk splits text into pieces of at most size bytes that overlap by about
// overlap bytes. It cuts at a blank line, then a newline, then a space, so
// chunks rarely split a sentence.
func Chunk(text string, size, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if overlap >= size/2 {
		overlap = size / 4
	}
	var out []string
	for start := 0; start < len(text); {
		end := start + size
		if end >= len(text) {
			out = append(out, strings.TrimSpace(text[start:]))
			break
		}
		end = cut(text, start+size/2, end)
		out = append(out, strings.TrimSpace(text[start:end]))
		next := cut(text, start+1, end-overlap)
		if next <= start {
			next = end
		}
		start = next
	}
	return out
}

// cut returns the best break in text[lo:hi], or hi when there is none.
func cut(text string, lo, hi int) int {
	if lo >= hi {
		return hi
	}
	win := text[lo:hi]
	for _, sep := range []string{"\n\n", "\n", ". ", " "} {
		if i := strings.LastIndex(win, sep); i >= 0 {
			return lo + i + len(sep)
		}
	}
	return hi
}
