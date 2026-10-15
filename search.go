package main

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"
	"golang.org/x/text/unicode/norm"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Search: first by ID (tmdb, imdb, tvdb), then by name. For name search the plugin scores the candidates itself (SearchCandidate.score):
// the year of a TMDB movie search only affects ordering and does not filter, while a series' first_air_date_year filters out results ±1 year away, so the year is left to the scoring.

type tmdbSearchResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	Popularity    float64 `json:"popularity"`
	// The following are for scoring hinted search: genre codes (16 is animation), original language, and a series' production countries.
	GenreIDs      []int    `json:"genre_ids"`
	OriginalLang  string   `json:"original_language"`
	OriginCountry []string `json:"origin_country"`
}

// goodMatch is the threshold for "the title counts as matched": below it, search again in the other Chinese script and compare the aliases and translated names of the top few results (see withAliases).
const goodMatch = 0.8

// aliasCandidates is the number of candidates whose aliases and per-language translated names are compared when the name did not match (ordered by current score, ties keeping TMDB's order).
const aliasCandidates = 3

// Search implements pluginv1.MetadataProviderServer.
func (p *provider) Search(ctx context.Context, req *pluginv1.SearchRequest) (*pluginv1.SearchResponse, error) {
	c, adult, err := p.ready()
	if err != nil {
		return nil, err
	}
	kind, ok := map[pluginv1.ItemType]string{pluginv1.ItemType_ITEM_TYPE_MOVIE: "movie", pluginv1.ItemType_ITEM_TYPE_SERIES: "tv"}[req.GetType()]
	if !ok {
		return nil, invalidArgument("search supports movies and series only")
	}
	target := parseLocale(req.GetLanguages().GetLanguage())
	lang := target.String()

	// ID hint: locate directly, score 1.
	hits, err := p.searchByID(ctx, c, kind, lang, req.GetProviderIds())
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		return p.candidates(hits, req, func(tmdbSearchResult) float64 { return 1 }), nil
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return &pluginv1.SearchResponse{}, nil
	}
	if len(req.GetTitles()) > 0 {
		return p.searchHinted(ctx, c, adult, req, kind, lang) // hints for name recognition (see hints.go)
	}
	search := func(query string) ([]tmdbSearchResult, error) {
		q := url.Values{"query": {query}, "language": {lang}, "include_adult": {strconv.FormatBool(adult)}}
		if y := req.GetYear(); y > 0 && kind == "movie" {
			q.Set("year", strconv.Itoa(int(y))) // only affects ordering
		}
		var page struct {
			Results []tmdbSearchResult `json:"results"`
		}
		if err := c.get(ctx, "/search/"+kind, q, &page); err != nil {
			return nil, p.upstream(c, err, "search")
		}
		return page.Results, nil
	}
	if hits, err = search(name); err != nil {
		return nil, err
	}
	score := func(h tmdbSearchResult) float64 { return p.score(name, int(req.GetYear()), h) }
	// A Chinese title often has fuller translated names and aliases in the other script (TMDB's zh-CN data is the most complete): when it did not match, convert and search once more.
	if best := maxScore(hits, score); best < goodMatch && hasHan(name) {
		alt := p.fold.convert(name, locale{lang: "zh", region: "TW"}, locale{lang: "zh", region: "CN"})
		if target.script() == "Hans" {
			alt = p.fold.convert(name, locale{lang: "zh", region: "CN"}, locale{lang: "zh", region: "TW"})
		}
		if alt != name {
			more, err := search(alt)
			if err != nil {
				return nil, err
			}
			for _, h := range more {
				if !slices.ContainsFunc(hits, func(x tmdbSearchResult) bool { return x.ID == h.ID }) {
					hits = append(hits, h)
				}
			}
		}
	}
	// Still no match: TMDB's search matches aliases and per-language translated names, but the results only return the localized title and original title, so the score cannot see them.
	// Fetch the aliases and translated names once for each of the top few candidates (cached per ID) and compare against them too, taking the highest score. Items that already matched cost no extra requests.
	var extraAll map[int][]string
	if maxScore(hits, score) < goodMatch {
		extra := p.withAliases(ctx, c, kind, hits, score)
		extraAll = extra
		score = func(h tmdbSearchResult) float64 { return p.scoreWith(name, int(req.GetYear()), h, extra[h.ID]) }
	}
	if kind == "movie" {
		// Sequel guard: when the query carries a sequel number and none of the candidate's names has the same number, the score is pushed below the auto-apply threshold (see sequel.go).
		base := score
		score = func(h tmdbSearchResult) float64 {
			return sequelGuard(name, append([]string{first(h.Title, h.Name), first(h.OriginalTitle, h.OriginalName)}, extraAll[h.ID]...), base(h))
		}
	}
	return p.candidates(hits, req, score), nil
}

// withAliases fetches the aliases and translated names of the top aliasCandidates candidates (failures are skipped and do not affect the search).
func (p *provider) withAliases(ctx context.Context, c *tmdbClient, kind string, hits []tmdbSearchResult, score func(tmdbSearchResult) float64) map[int][]string {
	order := slices.Clone(hits)
	slices.SortStableFunc(order, func(a, b tmdbSearchResult) int { return cmp.Compare(score(b), score(a)) })
	if len(order) > aliasCandidates {
		order = order[:aliasCandidates]
	}
	extra := map[int][]string{}
	for _, h := range order {
		if names := p.aliasNames(ctx, c, kind, h.ID); len(names) > 0 {
			extra[h.ID] = names
		}
	}
	return extra
}

func (p *provider) searchByID(ctx context.Context, c *tmdbClient, kind, lang string, ids map[string]string) ([]tmdbSearchResult, error) {
	if id := ids["tmdb"]; id != "" {
		var r tmdbSearchResult
		err := c.get(ctx, "/"+kind+"/"+url.PathEscape(id), url.Values{"language": {lang}}, &r)
		switch {
		case err == nil:
			return []tmdbSearchResult{r}, nil
		case !errors.Is(err, errNotFound):
			return nil, p.upstream(c, err, "item")
		}
	}
	for _, src := range []struct{ key, source string }{{"imdb", "imdb_id"}, {"tvdb", "tvdb_id"}} {
		id := ids[src.key]
		if id == "" {
			continue
		}
		var found struct {
			Movies []tmdbSearchResult `json:"movie_results"`
			TV     []tmdbSearchResult `json:"tv_results"`
		}
		if err := c.get(ctx, "/find/"+url.PathEscape(id), url.Values{"external_source": {src.source}, "language": {lang}}, &found); err != nil {
			if errors.Is(err, errNotFound) {
				continue
			}
			return nil, p.upstream(c, err, "item")
		}
		hits := found.Movies
		if kind == "tv" {
			hits = found.TV
		}
		if len(hits) > 0 {
			return hits, nil
		}
	}
	return nil, nil
}

// candidates sorts by descending score (ties keep TMDB's order) and applies limit.
func (p *provider) candidates(hits []tmdbSearchResult, req *pluginv1.SearchRequest, score func(tmdbSearchResult) float64) *pluginv1.SearchResponse {
	type scored struct {
		h tmdbSearchResult
		s float64
	}
	list := make([]scored, len(hits))
	for i, h := range hits {
		list[i] = scored{h, score(h)}
	}
	slices.SortStableFunc(list, func(a, b scored) int { return cmp.Compare(b.s, a.s) })
	if n := int(req.GetLimit()); n > 0 && len(list) > n {
		list = list[:n]
	}
	resp := &pluginv1.SearchResponse{}
	for _, x := range list {
		c := candidate(x.h, req.GetType(), p.imageSizes().poster)
		c.Score = x.s
		resp.Candidates = append(resp.Candidates, c)
	}
	return resp
}

func candidate(h tmdbSearchResult, t pluginv1.ItemType, posterSize string) *pluginv1.SearchCandidate {
	return &pluginv1.SearchCandidate{Id: strconv.Itoa(h.ID), Type: t, Name: first(h.Title, h.Name),
		OriginalName: first(h.OriginalTitle, h.OriginalName), Year: year(first(h.ReleaseDate, h.FirstAirDate)),
		Overview: h.Overview, ImageUrl: sizedURL(h.PosterPath, posterSize), ProviderIds: map[string]string{"tmdb": strconv.Itoa(h.ID)}}
}

func maxScore(hits []tmdbSearchResult, score func(tmdbSearchResult) float64) float64 {
	best := 0.0
	for _, h := range hits {
		best = max(best, score(h))
	}
	return best
}

// score is how well a candidate matches (0 to 1): the title score (the higher of translated name and original name) minus the penalties for year and missing poster.
func (p *provider) score(query string, wantYear int, h tmdbSearchResult) float64 {
	return p.scoreWith(query, wantYear, h, nil)
}

// scoreWith is the same as score, but also compares the title against extra (aliases and per-language translated names), taking the highest score; the year and poster penalties are unchanged.
func (p *provider) scoreWith(query string, wantYear int, h tmdbSearchResult, extra []string) float64 {
	s := max(p.titleScore(query, first(h.Title, h.Name)), p.titleScore(query, first(h.OriginalTitle, h.OriginalName)))
	for _, n := range extra {
		s = max(s, p.titleScore(query, n))
	}
	y := int(year(first(h.ReleaseDate, h.FirstAirDate)))
	switch {
	case wantYear == 0:
	case y == 0:
		s -= 0.1
	case y == wantYear:
	case y == wantYear-1 || y == wantYear+1:
		s -= 0.05 // release dates in different regions often straddle a year boundary
	default:
		s -= 0.2
	}
	if h.PosterPath == "" {
		s -= 0.01
	}
	return min(max(s, 0), 1)
}

// titleScore compares normalized titles: identical 1, prefix (by direction: the candidate has extra text beyond the query 0.85, the query has extra text beyond the candidate 0.95), contains 0.8, every query word appears 0.6, otherwise up to 0.5 by character bigram similarity.
func (p *provider) titleScore(query, title string) float64 {
	q, t := p.normalize(query), p.normalize(title)
	switch {
	case q == "" || t == "":
		return 0
	case q == t:
		return 1
	case strings.HasPrefix(t, q):
		return 0.85 // the candidate has extra text beyond the query (sequel number, subtitle): 0.15 below identical, so the runner-up is not too close
	case strings.HasPrefix(q, t):
		return 0.95 // the query has extra text beyond the candidate (the directory name has a foreign-language name or a note mixed in): the candidate is a prefix of the query, keeping the score from before 1.3.2
	case strings.Contains(t, q) || strings.Contains(q, t):
		return 0.8
	}
	words := strings.Fields(strings.ToLower(norm.NFKC.String(query)))
	if len(words) > 1 && !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(t, p.normalize(w)) }) {
		return 0.6
	}
	return 0.5 * dice(q, t)
}

// normalize is for comparison: NFKC (full-width to half-width), lowercase, Chinese unified to Simplified, keeping only letters and digits.
func (p *provider) normalize(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	if hasHan(s) {
		s = p.fold.convert(s, locale{lang: "zh", region: "TW"}, locale{lang: "zh", region: "CN"})
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return -1
	}, s)
}

// dice is the Dice coefficient of the character bigrams of two strings.
func dice(a, b string) float64 {
	grams := func(s string) map[string]int {
		r := []rune(s)
		m := map[string]int{}
		for i := 0; i+1 < len(r); i++ {
			m[string(r[i:i+2])]++
		}
		return m
	}
	ga, gb := grams(a), grams(b)
	total, common := 0, 0
	for g, n := range ga {
		total += n
		common += min(n, gb[g])
	}
	for _, n := range gb {
		total += n
	}
	if total == 0 {
		return 0
	}
	return 2 * float64(common) / float64(total)
}

// ---- aliases and translated names ----

const (
	aliasCacheTTL     = 30 * time.Minute
	aliasCacheEntries = 512
)

type aliasEntry struct {
	names   []string
	expires time.Time
}

// aliasCache is the in-memory cache of candidate aliases and translated names (keyed by kind and ID, content independent of language); errors are not cached.
type aliasCache struct {
	mu      sync.Mutex
	entries map[string]aliasEntry
	group   singleflight.Group
}

// aliasNames returns a candidate's aliases (alternative_titles) and per-language translated names (translations); nil when they cannot be fetched.
// One request brings back both; the same ID is not requested again within the TTL, and concurrent requests for the same ID are merged.
func (p *provider) aliasNames(ctx context.Context, c *tmdbClient, kind string, id int) []string {
	key := kind + "|" + strconv.Itoa(id)
	ac := &p.aliases
	ac.mu.Lock()
	if e, ok := ac.entries[key]; ok && time.Now().Before(e.expires) {
		ac.mu.Unlock()
		return e.names
	}
	ac.mu.Unlock()
	v, err, _ := ac.group.Do(key, func() (any, error) {
		var it struct {
			AltTitles    *tmdbAltTitles    `json:"alternative_titles"`
			Translations *tmdbTranslations `json:"translations"`
		}
		q := url.Values{"append_to_response": {"alternative_titles,translations"}}
		if err := c.get(ctx, "/"+kind+"/"+strconv.Itoa(id), q, &it); err != nil {
			return nil, err
		}
		var names []string
		for _, a := range it.AltTitles.all() {
			if t := strings.TrimSpace(a.Title); t != "" {
				names = append(names, t)
			}
		}
		if it.Translations != nil {
			for _, t := range it.Translations.Translations {
				if n := strings.TrimSpace(t.title()); n != "" {
					names = append(names, n)
				}
			}
		}
		ac.mu.Lock()
		defer ac.mu.Unlock()
		if ac.entries == nil || len(ac.entries) >= aliasCacheEntries {
			ac.entries = map[string]aliasEntry{} // rarely full: when full, clear everything
		}
		ac.entries[key] = aliasEntry{names: names, expires: time.Now().Add(aliasCacheTTL)}
		return names, nil
	})
	if err != nil {
		return nil
	}
	return v.([]string)
}
