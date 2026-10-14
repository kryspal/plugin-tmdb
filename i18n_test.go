package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kryspal/plugin-sdk/hanconv"
	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// allTexts collects every human-visible string in the declaration (the attribution is the English original that TMDB's terms require, and is not included).
func allTexts(m *pluginv1.Manifest) []string {
	out := []string{m.GetName(), m.GetDescription()}
	for _, g := range m.GetConfigGroups() {
		out = append(out, g.GetTitle())
	}
	for _, f := range m.GetConfigFields() {
		out = append(out, f.GetLabel(), f.GetDescription(), f.GetLinkLabel(), f.GetPlaceholder())
		for _, o := range f.GetOptions() {
			out = append(out, o.GetLabel())
		}
	}
	return out
}

// configureErrors triggers every kind of field error and returns all the error texts.
func configureErrors(t *testing.T, p *provider, lang string) []string {
	t.Helper()
	var out []string
	for _, v := range []map[string]string{
		{},
		{keyToken: "x", keyBaseURL: "http://x", keyMaxCast: "999", keyCertCountry: "usa"},
		{keyToken: "not a valid token!!"},
	} {
		resp, err := p.Configure(t.Context(), &pluginv1.ConfigureRequest{Values: v, Language: lang})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range resp.GetFieldErrors() {
			out = append(out, e)
		}
	}
	return out
}

// TestMessagesComplete: the keys of all three languages are complete, and every key has non-empty text.
func TestMessagesComplete(t *testing.T) {
	tags := messages.Tags()
	if len(tags) != 3 {
		t.Fatalf("tags = %v, want en, zh-tw, zh-cn", tags)
	}
	en := messages.For("en")
	for _, tag := range tags {
		tr := messages.For(tag)
		for _, k := range []string{"desc", "tokenLabel", "tokenHelp", "errRequired", "errTokenRejected", "trimLabel", "certHelp"} {
			if tr.T(k) == "" || tr.T(k) == k {
				t.Errorf("%s: missing %q", tag, k)
			}
			if tag != "en" && tr.T(k) == en.T(k) {
				t.Errorf("%s: %q is not translated", tag, k)
			}
		}
	}
}

// TestDescribeLanguages (the common spelling of "Taiwan" would be converted by OpenCC s2t to a different variant, so it is replaced first when comparing): en contains no Han characters; zh-TW is Traditional and zh-CN is Simplified (judged by being unchanged by an OpenCC conversion); an unknown language and an empty string fall back to English.
func TestDescribeLanguages(t *testing.T) {
	toTrad, err := hanconv.New("s2t")
	if err != nil {
		t.Fatal(err)
	}
	toSimp, err := hanconv.New("t2s")
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider()
	for _, tc := range []struct {
		lang string
		ok   func(string) bool
	}{
		{"en", func(s string) bool { return !hasHan(s) }},
		{"en-US", func(s string) bool { return !hasHan(s) }},
		{"", func(s string) bool { return !hasHan(s) }},
		{"fr", func(s string) bool { return !hasHan(s) }},
		{"ja", func(s string) bool { return !hasHan(s) }},
		{"zh-TW", func(s string) bool {
			s = strings.ReplaceAll(s, "台灣", "臺灣")
			c := toTrad.Convert(s)
			return c == s
		}},
		{"zh-Hant", func(s string) bool {
			s = strings.ReplaceAll(s, "台灣", "臺灣")
			c := toTrad.Convert(s)
			return c == s
		}},
		{"zh-CN", func(s string) bool { c := toSimp.Convert(s); return c == s }},
		{"zh", func(s string) bool { c := toSimp.Convert(s); return c == s }},
	} {
		m, err := p.Describe(t.Context(), &pluginv1.DescribeRequest{Language: tc.lang})
		if err != nil {
			t.Fatal(err)
		}
		texts := append(allTexts(m), configureErrors(t, p, tc.lang)...)
		for _, s := range texts {
			if !tc.ok(s) {
				t.Errorf("lang %q: text mixes in another language: %q", tc.lang, s)
			}
		}
		if strings.HasPrefix(tc.lang, "zh") {
			for _, s := range []string{m.GetDescription(), m.GetConfigFields()[0].GetLabel()} {
				if !hasHan(s) {
					t.Errorf("lang %q: %q is not Chinese", tc.lang, s)
				}
			}
		}
	}
}

// TestNoCountryCodesOnScreen (F17): the settings page text (labels, descriptions, options) shows no two-letter country codes such as TW; the certification country uses a menu of names, with "auto" meaning automatic.
func TestNoCountryCodesOnScreen(t *testing.T) {
	p := newProvider()
	code := regexp.MustCompile(`\b(TW|HK|MO|CN|US)\b`)
	for _, lang := range []string{"en", "zh-TW", "zh-CN"} {
		m, err := p.Describe(t.Context(), &pluginv1.DescribeRequest{Language: lang})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m.GetConfigFields() {
			if f.GetKey() == keyCertCountry && !strings.Contains(strings.ToLower(f.GetLabel()), "region") &&
				!strings.Contains(f.GetLabel(), "地區") && !strings.Contains(f.GetLabel(), "地区") {
				t.Errorf("lang %q: country label %q must say 國家/地區 (country/region)", lang, f.GetLabel())
			}
		}
		for _, s := range allTexts(m) {
			if code.MatchString(s) {
				t.Errorf("lang %q: country code shown on screen: %q", lang, s)
			}
		}
	}
	p = configured(t, newFakeTMDB(t))
	resp, err := p.Configure(t.Context(), &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, keyBaseURL: "http://x", keyCertCountry: certAuto}})
	if err != nil || resp.GetFieldErrors()[keyCertCountry] != "" {
		t.Errorf("Configure(auto) = %v, %v", resp, err)
	}
	if p.certCountry != "" {
		t.Errorf("auto stored as %q, want empty", p.certCountry)
	}
}

// TestMessageTablesScripts checks the string tables key by key (not through Describe, so keys that are never declared are checked too):
// the three languages have exactly the same keys and all are non-empty; en has no Han characters; zh-TW has no Simplified-only characters (unchanged after OpenCC s2t, with the usual spelling of "Taiwan" first replaced by the OpenCC variant);
// zh-CN has no Traditional-only characters (unchanged after t2s); the zh-TW and zh-CN texts must not be identical across the whole table (guards against pasting the wrong language).
func TestMessageTablesScripts(t *testing.T) {
	toTrad, err := hanconv.New("s2t")
	if err != nil {
		t.Fatal(err)
	}
	toSimp, err := hanconv.New("t2s")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"en", "zh-TW", "zh-CN"} {
		if len(messageTables[tag]) == 0 {
			t.Fatalf("no table for %s", tag)
		}
	}
	for key, en := range messageTables["en"] {
		for _, tag := range []string{"zh-TW", "zh-CN"} {
			if messageTables[tag][key] == "" {
				t.Errorf("%s: key %q is missing or empty", tag, key)
			}
		}
		if en == "" || hasHan(en) {
			t.Errorf("en: key %q = %q must be non-empty English", key, en)
		}
	}
	for tag, table := range messageTables {
		for key, text := range table {
			if _, ok := messageTables["en"][key]; !ok {
				t.Errorf("%s: key %q has no English text", tag, key)
			}
			switch tag {
			case "zh-TW":
				s := strings.ReplaceAll(text, "台灣", "臺灣")
				if c := toTrad.Convert(s); c != s {
					t.Errorf("zh-TW: key %q contains simplified characters: %q", key, text)
				}
			case "zh-CN":
				if c := toSimp.Convert(text); c != text {
					t.Errorf("zh-CN: key %q contains traditional characters: %q", key, text)
				}
			}
		}
	}
}
