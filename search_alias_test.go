package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Alias and prefix tests for search scoring . All use synthetic titles, with no names from a real library.

type aliasTMDB struct {
	*httptest.Server
	mu      sync.Mutex
	details map[string]int // number of detail requests (/movie/{id}, /tv/{id})
	results map[string][]map[string]any
	alts    map[string]map[string]any // key kind/id → detail response
}

func newAliasTMDB(t *testing.T) *aliasTMDB {
	f := &aliasTMDB{details: map[string]int{}, results: map[string][]map[string]any{}, alts: map[string]map[string]any{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/3")
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case path == "/authentication":
			write(map[string]any{"success": true})
		case strings.HasPrefix(path, "/search/"):
			f.mu.Lock()
			res := f.results[strings.TrimPrefix(path, "/search/")+"|"+r.URL.Query().Get("query")]
			f.mu.Unlock()
			if res == nil {
				res = []map[string]any{}
			}
			write(map[string]any{"results": res})
		default:
			f.mu.Lock()
			f.details[path]++
			d, ok := f.alts[path]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Query().Get("append_to_response") != "alternative_titles,translations" {
				t.Errorf("detail request without the expected append_to_response: %s", r.URL.RawQuery)
			}
			write(d)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *aliasTMDB) detailCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.details[path]
}

func (f *aliasTMDB) provider(t *testing.T) *provider {
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

func result(id int, title, original, date string) map[string]any {
	return map[string]any{"id": id, "title": title, "original_title": original, "release_date": date, "poster_path": "/p.jpg"}
}

func scoreOf(r *pluginv1.SearchResponse, id int) float64 {
	for _, c := range r.GetCandidates() {
		if c.GetId() == strconv.Itoa(id) {
			return math.Round(c.GetScore()*100) / 100
		}
	}
	return -1
}

// TestSearchUsesAliases: when the first result is the answer but its localized title and original title both fail to match, an alias or translated name makes it match (details of the top 3 are fetched, the same ID is not requested twice).
func TestSearchUsesAliases(t *testing.T) {
	f := newAliasTMDB(t)
	// The query is the Taiwan title; TMDB's localized title (en) and original title (Japanese) both fail to match, and the Taiwan title exists only in the aliases.
	f.results["movie|Zorblax Chronicle"] = []map[string]any{
		result(1, "The Quenwick Journey", "クェンウィックの旅", "2010-05-01"),
		result(2, "Another Film", "Another Film", "2011-01-01"),
		result(3, "Third Film", "Third Film", "2012-01-01"),
		result(4, "Fourth Film", "Fourth Film", "2013-01-01"),
	}
	f.alts["/movie/1"] = map[string]any{"id": 1,
		"alternative_titles": map[string]any{"titles": []any{map[string]any{"iso_3166_1": "TW", "title": "Zorblax Chronicle"}}}}
	// Candidates beyond the top 3 are not looked up (a 4th result does not count even if it has an alias).
	f.alts["/movie/4"] = map[string]any{"id": 4,
		"alternative_titles": map[string]any{"titles": []any{map[string]any{"iso_3166_1": "TW", "title": "Zorblax Chronicle"}}}}
	p := f.provider(t)
	ctx := context.Background()
	req := &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Chronicle", Year: 2010}
	r, err := p.Search(ctx, req)
	if err != nil || len(r.GetCandidates()) != 4 {
		t.Fatalf("Search = %v, %v", r, err)
	}
	if r.GetCandidates()[0].GetId() != "1" || scoreOf(r, 1) != 1 {
		t.Errorf("alias match: first = %s score %v, want id 1 score 1", r.GetCandidates()[0].GetId(), scoreOf(r, 1))
	}
	if s := scoreOf(r, 4); s >= goodMatch {
		t.Errorf("a fourth-place candidate was scored by its aliases: %v", s)
	}
	// Details are fetched only for the top 3; once each.
	for _, id := range []int{1, 2, 3} {
		if n := f.detailCount("/movie/" + strconv.Itoa(id)); n != 1 {
			t.Errorf("detail requests for %d = %d, want 1", id, n)
		}
	}
	if n := f.detailCount("/movie/4"); n != 0 {
		t.Errorf("detail requests for the fourth candidate = %d, want 0", n)
	}
	// Searching again within the same scrape (for example another language): cached, no repeated requests.
	if _, err := p.Search(ctx, req); err != nil {
		t.Fatal(err)
	}
	if n := f.detailCount("/movie/1"); n != 1 {
		t.Errorf("detail requests for 1 after a second search = %d, want 1 (cached)", n)
	}
}

// TestSearchUsesTranslations: translated names (translations) count too; a series' aliases are in results.
func TestSearchUsesTranslations(t *testing.T) {
	f := newAliasTMDB(t)
	f.results["movie|Plonk Zeta"] = []map[string]any{result(10, "Unrelated Words", "Unrelated Words", "2000-01-01")}
	f.alts["/movie/10"] = map[string]any{"id": 10, "translations": map[string]any{"translations": []any{
		map[string]any{"iso_639_1": "hi", "iso_3166_1": "IN", "data": map[string]any{"title": "Plonk Zeta"}}}}}
	f.results["tv|Quibble Show"] = []map[string]any{
		{"id": 20, "name": "Something Else", "original_name": "Something Else", "first_air_date": "2015-01-01", "poster_path": "/p.jpg"}}
	f.alts["/tv/20"] = map[string]any{"id": 20, "alternative_titles": map[string]any{"results": []any{map[string]any{"iso_3166_1": "TW", "title": "Quibble Show"}}}}
	p := f.provider(t)
	ctx := context.Background()
	r, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Plonk Zeta", Year: 2000})
	if err != nil || scoreOf(r, 10) != 1 {
		t.Errorf("translation match: %v %v", r, err)
	}
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Name: "Quibble Show", Year: 2015})
	if err != nil || scoreOf(r, 20) != 1 {
		t.Errorf("tv alias match: %v %v", r, err)
	}
	// The year penalty is unchanged: the alias matched, but the year is far off.
	f.results["movie|Plonk Zeta"] = []map[string]any{result(10, "Unrelated Words", "Unrelated Words", "2000-01-01")}
	r, _ = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Plonk Zeta", Year: 2010})
	if s := scoreOf(r, 10); s != 0.8 {
		t.Errorf("alias match with a far year = %v, want 1 - 0.2", s)
	}
}

// TestSearchSkipsAliasesWhenMatched: when the name already matched, no extra requests are made; when details cannot be fetched, the search is unaffected.
func TestSearchSkipsAliasesWhenMatched(t *testing.T) {
	f := newAliasTMDB(t)
	f.results["movie|Plain Title"] = []map[string]any{result(30, "Plain Title", "Plain Title", "2001-01-01")}
	f.results["movie|Nowhere Name"] = []map[string]any{result(31, "Different", "Different", "2001-01-01")} // no details (404)
	p := f.provider(t)
	ctx := context.Background()
	r, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Plain Title", Year: 2001})
	if err != nil || scoreOf(r, 30) != 1 {
		t.Fatalf("Search = %v, %v", r, err)
	}
	if n := f.detailCount("/movie/30"); n != 0 {
		t.Errorf("%d detail requests for a title that already matched", n)
	}
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Nowhere Name", Year: 2001})
	if err != nil || len(r.GetCandidates()) != 1 || scoreOf(r, 31) >= goodMatch {
		t.Errorf("failed alias lookup must not break the search: %v %v", r, err)
	}
}

// TestPrefixScoreGap: identical and prefix differ by at least 0.1, so the runner-up (a prefix) is not judged uncertain for being too close.
func TestPrefixScoreGap(t *testing.T) {
	p := newProvider()
	exact := p.score("Gamma Prize", 2020, tmdbSearchResult{Title: "Gamma Prize", ReleaseDate: "2020-01-01", PosterPath: "/p"})
	prefix := p.score("Gamma Prize", 2020, tmdbSearchResult{Title: "Gamma Prize 2", ReleaseDate: "2020-01-01", PosterPath: "/p"})
	if exact-prefix < 0.1 {
		t.Errorf("exact %v vs prefix %v: the gap must be at least 0.1", exact, prefix)
	}
	if prefix < goodMatch {
		t.Errorf("a prefix match (%v) must still count as a good match (>= %v)", prefix, goodMatch)
	}
}

// TestPrefixScoreDirection: the prefix score depends on direction. A candidate with extra text beyond the query (sequel number, subtitle) scores 0.85; a query with extra text beyond the candidate (the directory name has a foreign-language name or a note mixed in)
// scores 0.95, which still passes the host's auto-apply threshold of 0.9, no worse than before 1.3.2.
func TestPrefixScoreDirection(t *testing.T) {
	p := newProvider()
	score := func(query, title string) float64 {
		return p.score(query, 2020, tmdbSearchResult{Title: title, ReleaseDate: "2020-01-01", PosterPath: "/p"})
	}
	if s := score("Gamma Prize", "Gamma Prize 2"); math.Abs(s-0.85) > 1e-9 {
		t.Errorf("candidate longer than the query: %v, want 0.85", s)
	}
	if s := score("Gamma Prize extra words", "Gamma Prize"); math.Abs(s-0.95) > 1e-9 {
		t.Errorf("query longer than the candidate: %v, want 0.95", s)
	}
	if s := score("Gamma Prize", "Gamma Prize"); s != 1 {
		t.Errorf("exact: %v", s)
	}
	// Search overall: extra text in the query does not block auto-apply (the first result 0.95 ≥ 0.9; the runner-up is a sequel at 0.85, and when the gap of 0.1 or more does not hold the host decides; only the scores are checked here).
	f := newAliasTMDB(t)
	f.results["movie|Gamma Prize extra words"] = []map[string]any{result(40, "Gamma Prize", "Gamma Prize", "2020-01-01")}
	r, err := f.provider(t).Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Gamma Prize extra words", Year: 2020})
	if err != nil || scoreOf(r, 40) != 0.95 {
		t.Errorf("search: %v %v", r, err)
	}
}
