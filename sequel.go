package main

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Sequel-number guard (movies). When the query carries a sequel number (`Infernal Affairs II`, or a Chinese title such as "Sherlock Holmes 2: A Game of Shadows") and none of the candidate's names has the same number,
// the candidate is most likely the first film: a first film with an identical title would score 1.0 and be auto-applied, which is the most dangerous mismatch. So such a candidate's score is capped below the auto-apply threshold.
//   - The candidate has the same number (Roman and Arabic numerals count as equal): as usual.
//   - None of the candidate's names has a number: at most 0.8.
//   - The candidate has a different number: at most 0.5.
//
// Only used for movies: a number at the end of a series title is usually the season (`Hanzawa Naoki 2` must find the whole series).

const (
	noSequelCap    = 0.8
	otherSequelCap = 0.5
)

var (
	reSequelCN   = regexp.MustCompile(`第\s*([0-9一二三四五六七八九十两兩]{1,3})\s*[部集章篇]$`)
	reSequelTail = regexp.MustCompile(`(?i)(?:^|[\s\p{Han}\p{Hiragana}\p{Katakana}])(?:(II|III|IV|VI|VII|VIII|IX|X|XI|XII)|(\d{1,2}))$`)
	romanValue   = map[string]int{"II": 2, "III": 3, "IV": 4, "VI": 6, "VII": 7, "VIII": 8, "IX": 9, "X": 10, "XI": 11, "XII": 12}
	cnDigits     = map[rune]int{'一': 1, '二': 2, '两': 2, '兩': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10}
)

// sequelNumber returns the sequel number of a title: an Arabic numeral 2-19 or Roman numeral II-XII at the end of the part before the subtitle (colon, dash), or the Chinese "No. N part/episode/chapter" form; 0 when there is none.
func sequelNumber(title string) int {
	base := strings.TrimSpace(norm.NFKC.String(title))
	for _, sep := range []string{":", " - ", "："} {
		if i := strings.Index(base, sep); i > 0 {
			base = strings.TrimSpace(base[:i])
		}
	}
	if m := reSequelCN.FindStringSubmatch(base); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			n = 0
			for _, r := range m[1] {
				d, ok := cnDigits[r]
				if !ok {
					return 0
				}
				if d == 10 {
					n = max(n, 1) * 10
				} else {
					n += d
				}
			}
		}
		if n >= 2 && n <= 19 {
			return n
		}
		return 0
	}
	if m := reSequelTail.FindStringSubmatch(base); m != nil {
		if m[2] != "" {
			if n, err := strconv.Atoi(m[2]); err == nil && n >= 2 && n <= 19 {
				return n
			}
		}
		if m[1] != "" {
			return romanValue[strings.ToUpper(m[1])]
		}
	}
	return 0
}

// sequelGuard applies the rule above: when query carries a number, it limits the score according to the candidate's names (localized title, original title, aliases and translated names).
func sequelGuard(query string, names []string, score float64) float64 {
	q := sequelNumber(query)
	if q == 0 {
		return score
	}
	other := false
	for _, n := range names {
		switch c := sequelNumber(n); {
		case c == q:
			return score
		case c != 0:
			other = true
		}
	}
	if other {
		return min(score, otherSequelCap)
	}
	return min(score, noSequelCap)
}
