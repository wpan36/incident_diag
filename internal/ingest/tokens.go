package ingest

import "math"

// The token estimate.
//
// There is no bge-m3 tokenizer for Go, and taking one would mean a dependency,
// a vocabulary file of roughly 17MB to distribute, and a second artifact to
// replace whenever the embedding model changes. The chunk target sits far
// enough below the model's 8192-token ceiling that an estimate wrong by a
// factor of two still fits, so an estimate over runes is enough.
//
// The CJK coefficient is a guess that has never been measured against a real
// corpus. It is recorded as a known limitation in the spec and should be
// calibrated once M12's evaluation set exists.
const (
	runesPerTokenCJK   = 1.5
	runesPerTokenOther = 4.0
)

// EstimateTokens approximates how many tokens s would become.
//
// The second rule is a catch-all rather than a rule about ASCII: Cyrillic,
// accented Latin and emoji would otherwise fall through a table written for two
// alphabets, and this function has to be total — it is the one every packing
// decision is made against.
//
// The result is rounded up, so a chunk is never reported as smaller than the
// estimate believes it to be.
func EstimateTokens(s string) int {
	var tokens float64
	for _, r := range s {
		if isCJK(r) {
			tokens += 1 / runesPerTokenCJK
		} else {
			tokens += 1 / runesPerTokenOther
		}
	}
	return int(math.Ceil(tokens))
}

// cjkRanges are the blocks whose runes carry roughly one-and-a-half characters
// per token rather than four. Japanese kana and Hangul are included because
// they tokenize like the ideographs do, not like Latin text, and the fullwidth
// forms because a fullwidth comma in a Chinese runbook is part of the same
// sentence as the characters around it.
var cjkRanges = [...][2]rune{
	{0x3000, 0x303F}, // CJK symbols and punctuation
	{0x3040, 0x309F}, // Hiragana
	{0x30A0, 0x30FF}, // Katakana
	{0x3400, 0x4DBF}, // CJK unified ideographs extension A
	{0x4E00, 0x9FFF}, // CJK unified ideographs
	{0xAC00, 0xD7AF}, // Hangul syllables
	{0xF900, 0xFAFF}, // CJK compatibility ideographs
	{0xFF00, 0xFFEF}, // Halfwidth and fullwidth forms
	{0x20000, 0x2A6DF},
	{0x2A700, 0x2EBEF},
}

func isCJK(r rune) bool {
	for _, rg := range cjkRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}
