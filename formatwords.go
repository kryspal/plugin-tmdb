package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Format words: wording in a title that marks "this is the movie edition" and is not part of the work's name. File names often say "name + year" while the TMDB title is "name Movie Edition: subtitle"
// (or the reverse); once the format words are removed both sides are the same name; a match reached this way is capped at editionCap and is not treated as identical.
// Only words that mark the format are removed, not words such as "compilation" or "special" that denote a different work (those are different items).
const editionCap = 0.95

var (
	// formatWordsCJK are the CJK format words (Simplified, Traditional, Japanese, Korean).
	formatWordsCJK = []string{"剧场版", "劇場版", "电影版", "電影版", "극장판"}
	// reFormatWordsLatin are the Latin-script format words: whole words, "the movie" and "movie".
	reFormatWordsLatin = regexp.MustCompile(`(?i)\b(?:the\s+)?movie\b`)
	// reFormatLeftover is the punctuation and whitespace left at the ends and in the middle after the format words are removed.
	reFormatLeftover = regexp.MustCompile(`[\s:：\-–—·・,，、]+`)
)

// stripFormatWords removes the format words from a title and returns the remaining text (punctuation and whitespace between words collapsed into a single space);
// it returns the original text and false when there are no format words or the result is too short (fewer than 3 characters, e.g. the title itself is "Movie").
func stripFormatWords(s string) (string, bool) {
	out := s
	for _, w := range formatWordsCJK {
		out = strings.ReplaceAll(out, w, " ")
	}
	out = reFormatWordsLatin.ReplaceAllString(out, " ")
	if out == s {
		return s, false
	}
	out = strings.TrimSpace(reFormatLeftover.ReplaceAllString(out, " "))
	if utf8.RuneCountInString(strings.ReplaceAll(out, " ", "")) < 3 {
		return s, false
	}
	return out, true
}
