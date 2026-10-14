package main

import (
	"regexp"
	"strconv"
	"strings"
)

// Choosing translated names. When the target language has no translation, TMDB returns the original-language title in title and name instead of an empty string, and the episode name is a placeholder such as
// "Episode N" (in Chinese or English); so do not check whether the field is empty, check whether translations has a translation for that language.

// tmdbTranslation is one entry of translations. Movies use title, the others use name; a person's biography is in biography.
type tmdbTranslation struct {
	Lang   string `json:"iso_639_1"`
	Region string `json:"iso_3166_1"`
	Data   struct {
		Title     string `json:"title"`
		Name      string `json:"name"`
		Overview  string `json:"overview"`
		Tagline   string `json:"tagline"`
		Biography string `json:"biography"`
	} `json:"data"`
}

type tmdbTranslations struct {
	Translations []tmdbTranslation `json:"translations"`
}

func (t tmdbTranslation) locale() locale { return locale{lang: t.Lang, region: t.Region} }

// title is a movie's title or another kind's name.
func (t tmdbTranslation) title() string { return first(t.Data.Title, t.Data.Name) }

// placeholderRE matches names with no content: "Episode 3", "Season 2", their Chinese equivalents (episode N, season N, in Arabic or Chinese numerals), and plain digits.
var placeholderRE = regexp.MustCompile(`(?i)^\s*(?:第\s*[0-9０-９一二三四五六七八九十百千零〇两兩]+\s*[集話话季回部]|` +
	`(?:episode|ep\.?|season|chapter|épisode|folge|capítulo|エピソード|에피소드)\s*[0-9０-９]+|[0-9０-９]+)\s*$`)

// isPlaceholder reports whether a name is a placeholder (treated as a missing translation).
func isPlaceholder(s string) bool { return placeholderRE.MatchString(s) }

// picked is the chosen text and its source language (used to decide whether Simplified/Traditional conversion is needed).
type picked struct {
	text string
	from locale
}

// pick finds the first usable value in translations along the fallback chain: same language and region first; when a language on the chain has no region (for example ja), matching the language is enough.
// usable decides whether a value is usable (non-empty, not a placeholder).
func pick(ts []tmdbTranslation, ch []locale, field func(tmdbTranslation) string, usable func(string) bool) (picked, bool) {
	for _, want := range ch {
		for _, t := range ts {
			if t.Lang != want.lang || want.region != "" && t.Region != want.region {
				continue
			}
			if v := strings.TrimSpace(field(t)); usable(v) {
				return picked{text: v, from: t.locale()}, true
			}
		}
	}
	return picked{}, false
}

func nonEmpty(s string) bool { return s != "" }

func realName(s string) bool { return s != "" && !isPlaceholder(s) }

// localize converts the chosen text to the target language's Chinese script (when needed).
func (p *provider) localize(v picked, target locale) string {
	return p.conv.convert(v.text, v.from, target)
}

// seasonName is the name generated from the target language when a season has no real translation. Season 0 is "Specials".
func seasonName(target locale, n int) string {
	switch {
	case target.lang == "zh" && n == 0 && target.script() == "Hant":
		return "特別篇"
	case target.lang == "zh" && n == 0:
		return "特别篇"
	case target.lang == "zh":
		return "第 " + strconv.Itoa(n) + " 季"
	case n == 0:
		return "Specials"
	}
	return "Season " + strconv.Itoa(n)
}

// collectionSuffixRE matches the common Chinese series suffix on collection names: "(series)" in full-width or half-width parentheses, or a trailing Collection (which must be preceded by whitespace).
var collectionSuffixRE = regexp.MustCompile(`(?i)(\s*[（(]\s*系列\s*[）)]|\s+Collection)\s*$`)

// collectionName returns the collection name; with trim_collection_suffix set the suffix is removed. When nothing is left, the original name is kept.
func (p *provider) collectionName(name string) string {
	if !p.trimSeries.Load() {
		return name
	}
	if t := collectionSuffixRE.ReplaceAllString(name, ""); strings.TrimSpace(t) != "" {
		return strings.TrimSpace(t)
	}
	return name
}
