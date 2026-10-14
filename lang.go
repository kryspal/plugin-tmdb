package main

import (
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/kryspal/plugin-sdk/hanconv"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Language handling (change request 0018 part D): metadata is stored once, scraped in the language chosen for the library, so the plugin turns the result into the target language before returning it:
// when the target language lacks a translated name it takes from the fallback chain, when Chinese lacks one it uses the conversion to the other Chinese script, and genres are looked up by genre ID in the plugin's name table.

// locale is TMDB's "language-region", for example zh-TW, en-US; the region may be empty.
type locale struct {
	lang   string // two lowercase letters
	region string // two uppercase letters; may be empty
}

func (l locale) String() string {
	if l.region == "" {
		return l.lang
	}
	return l.lang + "-" + l.region
}

// script returns the Chinese script: TW, HK and MO are Traditional (Hant), CN and SG are Simplified (Hans); an empty string for non-Chinese.
func (l locale) script() string {
	if l.lang != "zh" {
		return ""
	}
	switch l.region {
	case "TW", "HK", "MO":
		return "Hant"
	}
	return "Hans"
}

// parseLocale converts BCP 47 to TMDB's language-region; an empty string is en-US.
// When there is only a script and no region, the region is inferred from the script: zh-Hant is zh-TW, zh-Hans and a bare zh are zh-CN (TMDB treats zh as Simplified).
func parseLocale(tag string) locale {
	if tag == "" {
		return locale{lang: "en", region: "US"}
	}
	parts := strings.Split(strings.ReplaceAll(tag, "_", "-"), "-")
	l := locale{lang: strings.ToLower(parts[0])}
	for _, p := range parts[1:] {
		switch {
		case len(p) == 2:
			l.region = strings.ToUpper(p)
		case strings.EqualFold(p, "hant") && l.region == "":
			l.region = "TW"
		case strings.EqualFold(p, "hans") && l.region == "":
			l.region = "CN"
		}
	}
	if l.lang == "zh" && l.region == "" {
		l.region = "CN"
	}
	return l
}

// tmdbLanguage is the string form of parseLocale (TMDB's language parameter).
func tmdbLanguage(tag string) string { return parseLocale(tag).String() }

// chineseSiblings returns the fallback order for a Chinese target: regions with the same script first, the other script after.
// zh-TW：HK、MO、CN、SG；zh-HK：MO、TW、CN、SG；zh-CN：SG、TW、HK。
func chineseSiblings(l locale) []locale {
	zh := func(r ...string) []locale {
		out := make([]locale, len(r))
		for i, x := range r {
			out[i] = locale{lang: "zh", region: x}
		}
		return out
	}
	switch l.region {
	case "TW":
		return zh("HK", "MO", "CN", "SG")
	case "HK", "MO":
		return zh("HK", "MO", "TW", "CN", "SG")
	case "SG":
		return zh("CN", "TW", "HK")
	}
	return zh("SG", "TW", "HK")
}

// chain returns the fallback chain for a text field: the target language, (for Chinese) the other Chinese regions, the requested fallback languages, English. Deduplicated.
func chain(l *pluginv1.Languages) []locale {
	target := parseLocale(l.GetLanguage())
	out := []locale{target}
	add := func(x locale) {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	if target.lang == "zh" {
		for _, s := range chineseSiblings(target) {
			add(s)
		}
	}
	for _, f := range l.GetFallbacks() {
		add(parseLocale(f))
	}
	add(locale{lang: "en", region: "US"})
	return out
}

// sameLanguage is the part of the fallback chain in the same language as the target (including other regions for Chinese). For person names and season names only same-language translations are taken:
// in a Chinese library, replacing a season name with the English "The Long Night", or a person name with another foreign-language form, is meaningless.
func sameLanguage(ch []locale) []locale {
	var out []locale
	for _, l := range ch {
		if l.lang == ch[0].lang {
			out = append(out, l)
		}
	}
	return out
}

// converter does the Simplified/Traditional Chinese conversion; the converter is only built on first use (loading the OpenCC dictionaries takes about 50 ms).
type converter struct {
	mu   sync.Mutex
	conv map[string]*hanconv.Converter
	off  bool // true when the setting turns conversion off
}

func (c *converter) disabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.off
}

func (c *converter) setOff(off bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off = off
}

// convert converts Chinese from region from into the script of to (including regional usage). Returned as-is when the script is the same or the text is not Chinese.
//
//	Simplified→Traditional: target TW uses s2twp (Taiwan usage), HK and MO use s2hk.
//	Traditional→Simplified: source TW uses tw2sp, HK and MO use hk2s.
func (c *converter) convert(s string, from, to locale) string {
	if s == "" || from.lang != "zh" || to.lang != "zh" || from.script() == to.script() {
		return s
	}
	var name string
	switch {
	case to.script() == "Hant" && to.region == "TW":
		name = "s2twp"
	case to.script() == "Hant":
		name = "s2hk"
	case from.region == "TW":
		name = "tw2sp"
	default:
		name = "hk2s"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off {
		return s
	}
	if c.conv == nil {
		c.conv = map[string]*hanconv.Converter{}
	}
	cc, ok := c.conv[name]
	if !ok {
		var err error
		if cc, err = hanconv.New(name); err != nil {
			return s
		}
		c.conv[name] = cc
	}
	return cc.Convert(s)
}

// hasHan reports whether the string contains Han characters.
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
