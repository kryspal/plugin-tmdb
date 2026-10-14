package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

// Tests for the name-recognition hint search (change order 0121 phase 2). All titles are synthetic; none comes from a real library.

type hintTMDB struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string                    // "path?query" (in order)
	results  map[string][]map[string]any // kind|query -> search results
	details  map[string]map[string]any   // path (/movie/1) -> detail response
	fail     map[string]int              // search query string -> response status code
}

func newHintTMDB(t *testing.T) *hintTMDB {
	f := &hintTMDB{results: map[string][]map[string]any{}, details: map[string]map[string]any{}, fail: map[string]int{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/3")
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		f.mu.Lock()
		defer f.mu.Unlock()
		if path != "/authentication" {
			f.requests = append(f.requests, path+"?"+r.URL.Query().Encode())
		}
		switch {
		case path == "/authentication":
			write(map[string]any{"success": true})
		case strings.HasPrefix(path, "/search/"):
			q := r.URL.Query().Get("query")
			if code := f.fail[q]; code != 0 {
				if code == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(code)
				return
			}
			res := f.results[strings.TrimPrefix(path, "/search/")+"|"+q]
			if res == nil {
				res = []map[string]any{}
			}
			write(map[string]any{"results": res})
		default:
			d, ok := f.details[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			write(d)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *hintTMDB) provider(t *testing.T) *provider {
	t.Helper()
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.http = f.Client()
		c.limiter = 0
		c.retry = time.Millisecond
		return c
	}
	resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, keyBaseURL: f.URL + "/3"}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure = %v, %v", resp, err)
	}
	return p
}

func (f *hintTMDB) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *hintTMDB) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *hintTMDB) has(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.requests, func(r string) bool { return strings.Contains(r, sub) })
}

func movieHit(id int, title, original, date string, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "title": title, "original_title": original, "release_date": date, "poster_path": "/p.jpg", "popularity": 1.0}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func hint(text string, role pluginv1.TitleRole, w float64) *pluginv1.TitleHint {
	return &pluginv1.TitleHint{Text: text, Role: role, Weight: w}
}

const (
	rolePrimary = pluginv1.TitleRole_TITLE_ROLE_PRIMARY
	roleAlt     = pluginv1.TitleRole_TITLE_ROLE_ALTERNATE
	roleVariant = pluginv1.TitleRole_TITLE_ROLE_VARIANT
)

func search(t *testing.T, p *provider, req *pluginv1.SearchRequest) *pluginv1.SearchResponse {
	t.Helper()
	if req.Languages == nil {
		req.Languages = &pluginv1.Languages{Language: "en"}
	}
	resp, err := p.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return resp
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// Stop at the first match: the first query already has a candidate >= 0.9, so only one request is sent; movies carry the year when the year source is reliable.
func TestHintedSearchStopsWhenMatched(t *testing.T) {
	f := newHintTMDB(t)
	f.results["movie|Zorblax Chronicle"] = []map[string]any{movieHit(1, "Zorblax Chronicle", "Zorblax Chronicle", "2010-05-01", nil)}
	p := f.provider(t)
	resp := search(t, p, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Chronicle", Year: 2010,
		YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN,
		Titles:     []*pluginv1.TitleHint{hint("Zorblax Chronicle", rolePrimary, 1), hint("Other Name", roleAlt, 0.8)}})
	c := resp.GetCandidates()[0]
	if f.total() != 1 || !f.has("year=2010") {
		t.Errorf("requests = %v; want one search with the year", f.requests)
	}
	if round2(c.GetScore()) != 1 || c.GetMatchedTitle() != "Zorblax Chronicle" || !slices.Contains(c.GetReasons(), "title.exact") || !slices.Contains(c.GetReasons(), "year.exact") {
		t.Errorf("candidate = %v", c)
	}
	// The year source is inferred from context: no year (series never carry one anyway).
	f2 := newHintTMDB(t)
	f2.results["movie|Zorblax Chronicle"] = f.results["movie|Zorblax Chronicle"]
	search(t, f2.provider(t), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Chronicle", Year: 2010,
		YearSource: pluginv1.YearSource_YEAR_SOURCE_CONTEXT, Titles: []*pluginv1.TitleHint{hint("Zorblax Chronicle", rolePrimary, 1)}})
	if f2.has("year=") {
		t.Errorf("a context year must not be sent: %v", f2.requests)
	}
	// The same search a second time: taken from the cache, no further request.
	before := f.total()
	search(t, p, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Chronicle", Year: 2010,
		YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN, Titles: []*pluginv1.TitleHint{hint("Zorblax Chronicle", rolePrimary, 1)}})
	if f.total() != before {
		t.Errorf("the repeated search made %d new requests", f.total()-before)
	}
}

// Query plan: when the first title does not match, try in order without the year, the other titles (at most 2) and the Chinese variants; stop at a match; the total request count stays within the limit.
func TestHintedSearchPlan(t *testing.T) {
	f := newHintTMDB(t)
	// Only the English title can be found.
	f.results["movie|Quenwick Journey"] = []map[string]any{movieHit(7, "Quenwick Journey", "Quenwick Journey", "2010-01-01", nil)}
	p := f.provider(t)
	resp := search(t, p, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "星海漫游", Year: 2010, YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN,
		Titles: []*pluginv1.TitleHint{hint("星海漫游", rolePrimary, 1), hint("Quenwick Journey", roleAlt, 0.8), hint("Never Asked One", roleAlt, 0.7),
			hint("Never Asked Two", roleAlt, 0.6), hint("星海漫遊", roleVariant, 0.3)}})
	if got := resp.GetCandidates(); len(got) != 1 || got[0].GetId() != "7" || got[0].GetScore() < stopScore {
		t.Fatalf("candidates = %v", got)
	}
	// 1 with the year, 2 without the year, 3 the English title (matches, stop). "Never Asked" and the variants were not sent.
	want := []string{"/search/movie?include_adult=false&language=en&query=%E6%98%9F%E6%B5%B7%E6%BC%AB%E6%B8%B8&year=2010",
		"/search/movie?include_adult=false&language=en&query=%E6%98%9F%E6%B5%B7%E6%BC%AB%E6%B8%B8",
		"/search/movie?include_adult=false&language=en&query=Quenwick+Journey&year=2010"}
	_ = want
	if f.total() != 3 || f.count("/search/movie") != 3 || f.has("Never") || f.has("%E6%98%9F%E6%B5%B7%E6%BC%AB%E9%81%8A") {
		t.Errorf("requests = %v; want year query, no-year query, then the English title (and stop)", f.requests)
	}
	if !strings.Contains(f.requests[0], "year=2010") || strings.Contains(f.requests[1], "year=") || !strings.Contains(f.requests[2], "Quenwick") {
		t.Errorf("order = %v", f.requests)
	}

	// Nothing can be found: at most 2 other titles and at most 2 variants, and the total requests stay within the limit.
	g := newHintTMDB(t)
	search(t, g.provider(t), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Name: "星海漫游",
		Titles: []*pluginv1.TitleHint{hint("星海漫游", rolePrimary, 1), hint("Alt One", roleAlt, 0.8), hint("Alt Two", roleAlt, 0.7), hint("Alt Three", roleAlt, 0.6),
			hint("星海漫遊", roleVariant, 0.3), hint("星海", roleVariant, 0.2), hint("星海漫游 变体", roleVariant, 0.1)}})
	if n := g.count("/search/tv"); n != 1+2+2 || g.has("Alt+Three") || g.has("%E5%8F%98") {
		t.Errorf("search requests = %d (%v); want first title + 2 other titles + 2 variants", n, g.requests)
	}
	if g.total() > maxSearchRequests {
		t.Errorf("%d requests, more than the limit %d", g.total(), maxSearchRequests)
	}
}

// When nothing matched, the details of the top 3 are fetched: an alias matches, matched_title is the alias and reasons has title.alias; the details are cached.
func TestHintedSearchAliasAndInfoCache(t *testing.T) {
	f := newHintTMDB(t)
	f.results["movie|Zorblax Chronicle"] = []map[string]any{
		movieHit(1, "The Quenwick Journey", "クェンウィックの旅", "2010-05-01", nil),
		movieHit(2, "Another Film", "Another Film", "2011-01-01", nil)}
	f.details["/movie/1"] = map[string]any{"id": 1, "alternative_titles": map[string]any{"titles": []map[string]any{{"iso_3166_1": "TW", "title": "Zorblax Chronicle"}}},
		"translations": map[string]any{"translations": []map[string]any{}}, "runtime": 120}
	f.details["/movie/2"] = map[string]any{"id": 2}
	p := f.provider(t)
	req := &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Chronicle", Year: 2010, YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN,
		Titles: []*pluginv1.TitleHint{hint("Zorblax Chronicle", rolePrimary, 1)}}
	resp := search(t, p, req)
	c := resp.GetCandidates()[0]
	if c.GetId() != "1" || c.GetMatchedTitle() != "Zorblax Chronicle" || !slices.Contains(c.GetReasons(), "title.alias") || c.GetScore() < stopScore {
		t.Fatalf("candidate = %v", c)
	}
	if f.count("/movie/") != 2 || !f.has("append_to_response=alternative_titles%2Ctranslations%2Crelease_dates") {
		t.Errorf("requests = %v", f.requests)
	}
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
	req.Limit = 1
	req.Titles = []*pluginv1.TitleHint{hint("Zorblax Chronicle", rolePrimary, 1), hint("Extra", roleAlt, 0.5)}
	search(t, p, req) // the search results and the details both come from the cache (Extra is another query and adds one search)
	if f.count("/movie/") != 0 {
		t.Errorf("the details were requested again: %v", f.requests)
	}
}

func TestYearAdjust(t *testing.T) {
	paren, ctxSrc, none := pluginv1.YearSource_YEAR_SOURCE_PAREN, pluginv1.YearSource_YEAR_SOURCE_CONTEXT, pluginv1.YearSource_YEAR_SOURCE_NONE
	for name, c := range map[string]struct {
		want    int
		src     pluginv1.YearSource
		y       int
		release []int
		strong  bool
		delta   float64
		reason  string
	}{
		"exact":                      {2010, paren, 2010, nil, true, 0, "year.exact"},
		"one year":                   {2010, paren, 2011, nil, true, -0.03, "year.diff1"},
		"two years, strong title":    {2010, paren, 2012, nil, true, -0.06, "year.diff2"},
		"two years, weak title":      {2010, paren, 2012, nil, false, -0.15, "year.diff2"},
		"three years":                {2010, paren, 2013, nil, true, -0.3, "year.far"},
		"release date in any region": {2010, paren, 2012, []int{2012, 2010}, false, 0, "year.release_date"},
		"no source":                  {2010, none, 2020, nil, false, 0, "year.unknown"},
		"no wanted year":             {0, paren, 2020, nil, false, 0, "year.unknown"},
		"candidate without a year":   {2010, paren, 0, nil, true, -0.1, "year.missing"},
		"context halves the penalty": {2010, ctxSrc, 2013, nil, true, -0.15, "year.far"},
	} {
		d, r := yearAdjust(c.want, c.src, c.y, c.release, c.strong)
		if round2(d) != c.delta || r != c.reason {
			t.Errorf("%s: %.2f %s, want %.2f %s", name, d, r, c.delta, c.reason)
		}
	}
}

func TestFoldSequelAndSegments(t *testing.T) {
	for in, want := range map[string]string{"Rocky II": "Rocky 2", "Rocky Ⅲ": "Rocky 3", "星海漫游第二部": "星海漫游2", "无间道三": "无间道3", "Mr. X": "Mr. X", "Alpha": "Alpha"} {
		if got := foldSequel(in); got != want {
			t.Errorf("foldSequel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := scriptSegments("星海漫游 Quenwick Journey"); !slices.Equal(got, []string{"星海漫游", "Quenwick Journey"}) {
		t.Errorf("segments = %q", got)
	}
	p := newProvider()
	// Sequel number normalization, one segment of a mixed Chinese-English title, the variant cap.
	if s, _ := p.hintTitleScore(hintTitle{text: "Rocky II", role: rolePrimary}, "Rocky 2"); s != 1 {
		t.Errorf("Rocky II vs Rocky 2 = %v", s)
	}
	if s, _ := p.hintTitleScore(hintTitle{text: "无间道二", role: rolePrimary}, "无间道2"); s != 1 {
		t.Errorf("trailing Chinese numeral = %v", s)
	}
	if s, _ := p.hintTitleScore(hintTitle{text: "星海漫游 Quenwick Journey", role: rolePrimary}, "Quenwick Journey"); s != segmentCap {
		t.Errorf("mixed title, one segment = %v, want the segment cap %v", s, segmentCap)
	}
	if s, capped := p.hintTitleScore(hintTitle{text: "Quenwick Journey", role: roleVariant}, "Quenwick Journey"); s != variantCap || !capped {
		t.Errorf("variant = %v %v, want the variant cap", s, capped)
	}
}

// Scoring features: anime genre, region and language, runtime, shape, sequel guard; reason codes.
func TestHintScoreFeatures(t *testing.T) {
	p := newProvider()
	titles := []hintTitle{{text: "Quenwick Journey", role: rolePrimary, weight: 1}}
	base := func() tmdbSearchResult {
		return tmdbSearchResult{ID: 1, Title: "Quenwick Journey", OriginalTitle: "Quenwick Journey", ReleaseDate: "2010-01-01", PosterPath: "/p.jpg"}
	}
	req := func(mod func(*pluginv1.SearchRequest)) *pluginv1.SearchRequest {
		r := &pluginv1.SearchRequest{Name: "Quenwick Journey", Year: 2010, YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN}
		if mod != nil {
			mod(r)
		}
		return r
	}
	score := func(r *pluginv1.SearchRequest, kind string, h tmdbSearchResult, info *titleInfo) scored {
		return p.hintScore(r, kind, titles, h, info)
	}
	plain := score(req(nil), "movie", base(), nil)
	if round2(plain.score) != 1 {
		t.Fatalf("baseline = %v", plain)
	}
	lower := func(r *pluginv1.SearchRequest) *pluginv1.SearchRequest { r.Year = 2011; return r } // lowers the baseline below 1 so a bonus is visible
	_ = lower
	// Anime flag: the animation genre gives a bonus (first push the baseline down to 0.9, then compare), its absence a deduction.
	weak := tmdbSearchResult{ID: 1, Title: "Quenwick Journey and more", OriginalTitle: "Quenwick Journey and more", ReleaseDate: "2010-01-01", PosterPath: "/p.jpg"}
	anime := func(r *pluginv1.SearchRequest) { r.Flags = []string{"anime"} }
	w0 := score(req(nil), "tv", weak, nil)
	weakAnime := weak
	weakAnime.GenreIDs = []int{16, 18}
	if got := score(req(anime), "tv", weakAnime, nil); round2(got.score-w0.score) != 0.02 || !slices.Contains(got.reasons, "genre.anime") {
		t.Errorf("anime genre: %v vs %v", got, w0)
	}
	weakDrama := weak
	weakDrama.GenreIDs = []int{18}
	if got := score(req(anime), "tv", weakDrama, nil); round2(got.score-w0.score) != -0.05 || !slices.Contains(got.reasons, "genre.not_anime") {
		t.Errorf("not anime: %v vs %v", got, w0)
	}
	if got := score(req(anime), "tv", weak, nil); round2(got.score-w0.score) != 0 {
		t.Errorf("no genre data must not change the score: %v", got)
	}
	// Region and language: a match adds 0.03; no data, no deduction.
	region := func(r *pluginv1.SearchRequest) { r.RegionHints, r.LanguageHints = []string{"JP"}, []string{"ja"} }
	jp := weak
	jp.OriginalLang = "ja"
	if got := score(req(region), "tv", jp, nil); round2(got.score-w0.score) != 0.03 || !slices.Contains(got.reasons, "language.match") {
		t.Errorf("language: %v", got)
	}
	us := weak
	us.OriginalLang, us.OriginCountry = "en", []string{"JP"}
	if got := score(req(region), "tv", us, nil); round2(got.score-w0.score) != 0.03 || !slices.Contains(got.reasons, "region.match") {
		t.Errorf("region: %v", got)
	}
	// Runtime (movies, when there are details).
	dur := func(r *pluginv1.SearchRequest) { r.DurationSeconds = 7200 }
	m0 := score(req(nil), "movie", weak, &titleInfo{})
	if got := score(req(dur), "movie", weak, &titleInfo{runtime: 125}); round2(got.score-m0.score) != 0.03 || !slices.Contains(got.reasons, "runtime.ok") {
		t.Errorf("runtime ok: %v", got)
	}
	if got := score(req(dur), "movie", weak, &titleInfo{runtime: 60}); round2(got.score-m0.score) != -0.05 || !slices.Contains(got.reasons, "runtime.off") {
		t.Errorf("runtime off: %v", got)
	}
	// Shape (series): a fit adds, a clear mismatch deducts, and the absolute episode count of a single season fitting the whole series counts as fitting.
	shape := func(s *pluginv1.SeriesShape) func(*pluginv1.SearchRequest) {
		return func(r *pluginv1.SearchRequest) { r.Shape = s }
	}
	info := &titleInfo{seasons: map[int]int{1: 12, 2: 12}}
	ok := &pluginv1.SeriesShape{SeasonNumbers: []int32{1, 2}, EpisodesPerSeason: map[int32]int32{1: 12, 2: 10}, MaxEpisodeNumber: 12}
	if got := score(req(shape(ok)), "tv", weak, info); round2(got.score-w0.score) != 0.03 || !slices.Contains(got.reasons, "shape.ok") {
		t.Errorf("shape ok: %v", got)
	}
	bad := &pluginv1.SeriesShape{SeasonNumbers: []int32{1, 2, 3}, EpisodesPerSeason: map[int32]int32{1: 12, 2: 12, 3: 8}, MaxEpisodeNumber: 12}
	if got := score(req(shape(bad)), "tv", weak, info); round2(got.score-w0.score) != -0.1 || !slices.Contains(got.reasons, "shape.mismatch") {
		t.Errorf("shape mismatch: %v", got)
	}
	absolute := &pluginv1.SeriesShape{SeasonNumbers: []int32{1}, EpisodesPerSeason: map[int32]int32{1: 24}, MaxEpisodeNumber: 24}
	if got := score(req(shape(absolute)), "tv", weak, info); !slices.Contains(got.reasons, "shape.ok") {
		t.Errorf("a single season numbered continuously fits the whole series: %v", got)
	}
	tooMany := &pluginv1.SeriesShape{SeasonNumbers: []int32{1}, EpisodesPerSeason: map[int32]int32{1: 40}, MaxEpisodeNumber: 40}
	if got := score(req(shape(tooMany)), "tv", weak, info); !slices.Contains(got.reasons, "shape.mismatch") {
		t.Errorf("more episodes than the whole series: %v", got)
	}
	// Sequel guard (movies): the query carries a number and the candidate does not -> cap 0.8.
	seq := []hintTitle{{text: "Quenwick Journey II", role: rolePrimary, weight: 1}}
	r := req(nil)
	r.Name = "Quenwick Journey II"
	if got := p.hintScore(r, "movie", seq, base(), nil); got.score > noSequelCap || !slices.Contains(got.reasons, "sequel.cap") {
		t.Errorf("sequel guard: %v", got)
	}
	// Exception to the sequel guard: the number is only in another title (a Chinese title "Zorvik Quest 2" paired with the English "Quenwick Journey"), and the matched English title is identical and the year matches exactly -> no cap.
	both := []hintTitle{{text: "Quenwick Journey II", role: rolePrimary, weight: 1}, {text: "Quenwick Journey", role: pluginv1.TitleRole_TITLE_ROLE_ALTERNATE, weight: 0.9}}
	if got := p.hintScore(r, "movie", both, base(), nil); got.score < stopScore || slices.Contains(got.reasons, "sequel.cap") || !slices.Contains(got.reasons, "sequel.exempt") {
		t.Errorf("exact title and year without a number must not be capped by another title's number: %v", got)
	}
	// When the year is unknown, does not match, or the title is only a prefix, the cap holds as before; so it does when the matched title carries the number itself.
	noYear := &pluginv1.SearchRequest{Name: r.Name, YearSource: pluginv1.YearSource_YEAR_SOURCE_NONE}
	if got := p.hintScore(noYear, "movie", both, base(), nil); got.score > noSequelCap || !slices.Contains(got.reasons, "sequel.cap") {
		t.Errorf("without a year the guard stays: %v", got)
	}
	oldFirst := base()
	oldFirst.ReleaseDate = "1995-01-01"
	if got := p.hintScore(r, "movie", both, oldFirst, nil); got.score >= stopScore || slices.Contains(got.reasons, "sequel.exempt") {
		t.Errorf("a different year is never exempt (and its score stays below the auto-apply bar): %v", got)
	}
	prefix := base()
	prefix.Title, prefix.OriginalTitle = "Quenwick Journey and More", "Quenwick Journey and More"
	if got := p.hintScore(r, "movie", both, prefix, nil); !slices.Contains(got.reasons, "sequel.cap") {
		t.Errorf("a title that is only a prefix match keeps the guard: %v", got)
	}
	// The upper bound is 1 and the lower bound is 0.
	if got := score(req(func(r *pluginv1.SearchRequest) { r.Year = 1950 }), "movie", base(), nil); got.score < 0 || got.score > 1 {
		t.Errorf("score out of range: %v", got.score)
	}
}

// On ties the more popular one goes first; a request without hints takes the legacy path (a single query, no matched_title).
func TestHintedPopularityTieBreakAndLegacyPath(t *testing.T) {
	f := newHintTMDB(t)
	f.results["movie|Zorblax"] = []map[string]any{
		movieHit(1, "Zorblax", "Zorblax", "2010-01-01", map[string]any{"popularity": 1.0}),
		movieHit(2, "Zorblax", "Zorblax", "2010-01-01", map[string]any{"popularity": 9.0})}
	p := f.provider(t)
	resp := search(t, p, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax", Titles: []*pluginv1.TitleHint{hint("Zorblax", rolePrimary, 1)}})
	if got := resp.GetCandidates(); got[0].GetId() != "2" || got[0].GetScore() != got[1].GetScore() {
		t.Errorf("candidates = %v; want the more popular one first with the same score", got)
	}
	legacy := search(t, p, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax"})
	if legacy.GetCandidates()[0].GetMatchedTitle() != "" || len(legacy.GetCandidates()[0].GetReasons()) != 0 {
		t.Errorf("the legacy path must not fill matched_title and reasons: %v", legacy.GetCandidates()[0])
	}
}

// If the first query fails an error is returned; if a later query fails it is skipped; rate limiting is passed up as usual.
func TestHintedSearchFailures(t *testing.T) {
	f := newHintTMDB(t)
	f.fail["boom"] = http.StatusInternalServerError
	p := f.provider(t)
	if _, err := p.Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "boom", Titles: []*pluginv1.TitleHint{hint("boom", rolePrimary, 1)}}); err == nil {
		t.Error("a failing first query must fail the search")
	}
	f.results["movie|First"] = []map[string]any{movieHit(1, "Weak Match", "Weak Match", "2010-01-01", nil)}
	f.fail["Second"] = http.StatusInternalServerError
	resp, err := p.Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "First", Titles: []*pluginv1.TitleHint{
		hint("First", rolePrimary, 1), hint("Second", roleAlt, 0.5)}})
	if err != nil || len(resp.GetCandidates()) != 1 {
		t.Errorf("a failing later query must not fail the search: %v %v", resp, err)
	}
	f.fail["Third"] = http.StatusTooManyRequests
	_, err = p.Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "First", Titles: []*pluginv1.TitleHint{
		hint("First", rolePrimary, 1), hint("Third", roleAlt, 0.5)}})
	if _, ok := errors.AsType[*sdk.RateLimitError](err); !ok {
		t.Errorf("a rate limit in a later query must reach the host: %v", err)
	}
}

// Absolute episode numbers: map back to (season, episode) by accumulating season episode counts; beyond the whole series return NOT_FOUND; with season and episode given it behaves as before; the series details are requested only once.
func TestAbsoluteEpisode(t *testing.T) {
	f := newHintTMDB(t)
	f.details["/tv/9"] = map[string]any{"id": 9, "name": "Show", "number_of_seasons": 3, "seasons": []map[string]any{
		{"season_number": 0, "episode_count": 4}, {"season_number": 1, "episode_count": 12}, {"season_number": 2, "episode_count": 13}, {"season_number": 3, "episode_count": 6}}}
	episodes := func(season int, n int) map[string]any {
		var eps []map[string]any
		for i := 1; i <= n; i++ {
			eps = append(eps, map[string]any{"id": season*100 + i, "name": fmt.Sprintf("Episode name %d-%d", season, i), "episode_number": i, "season_number": season, "air_date": "2020-01-01"})
		}
		return map[string]any{"id": season, "season_number": season, "episodes": eps}
	}
	f.details["/tv/9/season/1"], f.details["/tv/9/season/2"], f.details["/tv/9/season/3"] = episodes(1, 12), episodes(2, 13), episodes(3, 6)
	p := f.provider(t)
	get := func(abs int32) (*pluginv1.ItemMetadata, error) {
		return p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "9",
			AbsoluteEpisodeNumber: &abs, Languages: &pluginv1.Languages{Language: "en"}})
	}
	for abs, want := range map[int32][2]int32{1: {1, 1}, 12: {1, 12}, 13: {2, 1}, 25: {2, 13}, 26: {3, 1}, 31: {3, 6}} {
		m, err := get(abs)
		if err != nil || m.GetParentIndexNumber() != want[0] || m.GetIndexNumber() != want[1] {
			t.Errorf("absolute %d = %v, %v; want S%dE%d", abs, m, err, want[0], want[1])
		}
	}
	if _, err := get(32); err == nil || !strings.Contains(err.Error(), "NotFound") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("beyond the last episode = %v, want not found", err)
	}
	if _, err := get(0); err == nil {
		t.Error("absolute 0 must be rejected")
	}
	if n := f.count("/tv/9?"); n != 1 {
		t.Errorf("the series details were requested %d times, want 1 (cached)", n)
	}
	// With season and episode given: as before (the season/episode counts of the series details are not queried).
	s, e := int32(2), int32(3)
	m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "9", SeasonNumber: &s, EpisodeNumber: &e,
		Languages: &pluginv1.Languages{Language: "en"}})
	if err != nil || m.GetParentIndexNumber() != 2 || m.GetIndexNumber() != 3 {
		t.Errorf("season and episode = %v, %v", m, err)
	}
	if _, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "x", AbsoluteEpisodeNumber: proto32(3)}); err == nil {
		t.Error("a non-numeric series id must be rejected")
	}
}

func proto32(n int32) *int32 { return &n }

func TestDescribeDeclaresHintCapabilities(t *testing.T) {
	m, err := newProvider().Describe(context.Background(), &pluginv1.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Absolute episode numbers and hint search are both declared (phase 2 passed at full weight: 2026-10-12, A7 measured with the replay cache, 97/101, 0 wrong applications).
	if !slices.Contains(m.GetCapabilities(), pluginv1.CapabilityMetadataSeriesAbsolute) {
		t.Error("metadata.series.absolute is not declared")
	}
	if got := slices.Contains(m.GetCapabilities(), pluginv1.CapabilityMetadataSearchHints); !got {
		t.Errorf("metadata.search.hints declared = %v, want true", got)
	}
	if v, _ := strconv.Atoi(strings.Split(version(), ".")[0]); v < 2 {
		t.Errorf("version %s", version())
	}
}

// Rating source (change order 0122 section F): TMDB's score and vote count go in ratings (source tmdb, max 10), community_rating as before; not given when there is no score.
func TestRatingsAreReported(t *testing.T) {
	if got := tmdbRatings(7.45, 1234); len(got) != 1 || got[0].GetSource() != "tmdb" || got[0].GetValue() != 7.45 || got[0].GetMax() != 10 || got[0].GetVotes() != 1234 {
		t.Errorf("ratings = %v", got)
	}
	if got := tmdbRatings(0, 0); got != nil {
		t.Errorf("a title without a score = %v, want none", got)
	}
	if got := tmdbRatings(5, -3); got[0].GetVotes() != 0 {
		t.Errorf("negative votes = %v", got[0])
	}
	p := newProvider()
	m := p.titleMetadata(pluginv1.ItemType_ITEM_TYPE_MOVIE, tmdbItem{ID: 1, Title: "Zorblax", VoteAverage: 6.8, VoteCount: 90}, []locale{{lang: "en"}})
	if m.GetCommunityRating() != 6.8 || len(m.GetRatings()) != 1 || m.GetRatings()[0].GetVotes() != 90 {
		t.Errorf("title = %v %v", m.GetCommunityRating(), m.GetRatings())
	}
}

// Edition words (theatrical version, movie version, The Movie): a title that becomes identical after removing them is nearly identical (capped at editionCap); only words that mark an edition are removed,
// not words such as the Chinese "recap compilation" marker, which denote a different work, and a title is not stripped down to almost nothing.
func TestStripFormatWords(t *testing.T) {
	for in, want := range map[string]string{
		"Zorvik 剧场版：Aria Arc":       "Zorvik Aria Arc", // Simplified
		"Zorvik 劇場版：Aria Arc":       "Zorvik Aria Arc", // Traditional, Japanese
		"Zorvik 电影版 Aria Arc":       "Zorvik Aria Arc",
		"Zorvik 電影版 Aria Arc":       "Zorvik Aria Arc",
		"Zorvik 극장판 Aria Arc":       "Zorvik Aria Arc", // Korean
		"Zorvik: The Movie":         "Zorvik",
		"Zorvik the movie Aria Arc": "Zorvik Aria Arc",
		"星港剧场版：蕾塞篇":                 "星港 蕾塞篇",
		"Zorvik Aria Arc":           "",
		"Zorvik 总集篇":                "", // the recap-compilation marker denotes a different work
		"Movie":                     "",
		"Movie 43":                  "", // only two characters would be left after stripping
		"Moviegoers of Zorvik":      "", // not a whole word
	} {
		got, ok := stripFormatWords(in)
		if want == "" && ok || want != "" && (!ok || got != want) {
			t.Errorf("stripFormatWords(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestFormatWordsMakeTitlesNearlyExact(t *testing.T) {
	p := newProvider()
	for _, c := range []struct{ query, name string }{
		{"Zorvik Aria Arc", "Zorvik 劇場版：Aria Arc"},
		{"Zorvik 剧场版 Aria Arc", "Zorvik Aria Arc"},
		{"星港蕾塞篇", "星港剧场版：蕾塞篇"},
		{"星港蕾塞篇", "星港電影版：蕾塞篇"},
		{"Zorvik Aria Arc", "Zorvik: Aria Arc - The Movie"},
	} {
		got, capped := p.hintTitleScore(hintTitle{text: c.query, role: rolePrimary, weight: 1}, c.name)
		if got < 0.9 || got > editionCap || !capped && got != editionCap {
			t.Errorf("%q vs %q = %v (capped %v), want between 0.9 and %v", c.query, c.name, got, capped, editionCap)
		}
	}
	// Different titles do not become alike through edition words.
	if got, _ := p.hintTitleScore(hintTitle{text: "Zorvik Aria Arc", role: rolePrimary, weight: 1}, "Zorvik 剧场版：Other Arc"); got >= 0.9 {
		t.Errorf("a different subtitle scored %v", got)
	}
	// Full scoring: with a matching year it passes the auto-apply threshold of 0.9.
	r := &pluginv1.SearchRequest{Name: "星港蕾塞篇", Year: 2025, YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN}
	h := tmdbSearchResult{ID: 1, Title: "星港剧场版：蕾塞篇", OriginalTitle: "Zorvik the Movie: Reze", ReleaseDate: "2025-09-19", PosterPath: "/p.jpg"}
	if got := p.hintScore(r, "movie", []hintTitle{{text: "星港蕾塞篇", role: rolePrimary, weight: 1}}, h, nil); got.score < 0.9 || got.score > editionCap+0.001 || !slices.Contains(got.reasons, "year.exact") {
		t.Errorf("hintScore = %+v", got)
	}
}

// When a same-title rival has an exact year match, the one that is 1 to 2 years off is deducted a further twinPenalty; with only one, or no rival whose year matches exactly, nothing changes.
func TestYearTwinsArePenalized(t *testing.T) {
	p := newProvider()
	titles := []hintTitle{{text: "Quenwick", role: rolePrimary, weight: 1}}
	r := &pluginv1.SearchRequest{Name: "Quenwick", Year: 2025, YearSource: pluginv1.YearSource_YEAR_SOURCE_PAREN}
	mk := func(id int, date string) scored {
		return p.hintScore(r, "movie", titles, tmdbSearchResult{ID: id, Title: "Quenwick", OriginalTitle: "Quenwick", ReleaseDate: date, PosterPath: "/p.jpg"}, nil)
	}
	exact, near, far, mid := mk(1, "2025-07-01"), mk(2, "2023-03-01"), mk(3, "1978-12-15"), mk(4, "2024-05-05")
	alone := mk(2, "2023-03-01")
	if alone.score != near.score || near.score < 0.9 {
		t.Fatalf("test setup: a lone 2-year-off exact title should still pass the bar (%v)", near.score)
	}
	got := penalizeYearTwins([]scored{exact, near, far, mid})
	if got[0].score != exact.score || got[2].score != far.score {
		t.Errorf("the exact-year and far candidates must not change: %+v", got)
	}
	if want := near.score - twinPenalty; got[1].score != want || !slices.Contains(got[1].reasons, "year.twin") || exact.score-got[1].score < 0.1 {
		t.Errorf("2 years off = %v (%v), want %v and a lead of at least 0.1 for the exact-year candidate", got[1].score, got[1].reasons, want)
	}
	if want := mid.score - twinPenalty; got[3].score != want {
		t.Errorf("1 year off = %v, want %v", got[3].score, want)
	}
	// No rival whose year matches exactly: unchanged.
	if g := penalizeYearTwins([]scored{near, mid, far}); g[0].score != near.score || g[1].score != mid.score {
		t.Errorf("without an exact-year twin nothing changes: %+v", g)
	}
	// Only one: unchanged; the input reasons are not modified.
	if g := penalizeYearTwins([]scored{alone}); g[0].score != alone.score || slices.Contains(g[0].reasons, "year.twin") {
		t.Errorf("a lone candidate changed: %+v", g)
	}
	if slices.Contains(near.reasons, "year.twin") {
		t.Error("penalizeYearTwins modified a reasons slice it was given")
	}
}
