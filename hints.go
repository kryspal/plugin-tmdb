package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"
	"golang.org/x/text/unicode/norm"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

// Hint search for name recognition. The host brings the hints for name
// recognition in a SearchRequest (several titles, year source, region and language hints, the local season/episode structure, flags, runtime) and it comes here; a request without hints goes through the single-title search in search.go as before.
//
// Query plan (stops at the first match; one Search sends at most maxSearchRequests name searches and detail requests; ID lookups are not counted):
//  1. The first title; for movies, with the year when the year source is reliable (parentheses, standalone token).
//  2. The first title without the year (when step 1 carried a year and found no match).
//  3. The other titles (at most 2, by weight).
//  4. Variants of a Chinese title: the other script, subtitle stripped and so on (at most 2).
//  5. Aliases, per-language translations, release dates, runtime and the season/episode counts of series for the top 3 (when nothing matched, or the top few cannot be told apart), cached for 30 minutes.
//
// See hintScore for the scoring features and deductions; a candidate's matched_title is the name that matched, and reasons are the per-feature codes.

const (
	// stopScore means "matched": the query plan stops here.
	stopScore = 0.9
	// maxSearchRequests is the most name searches and detail requests one hint search sends (cache hits are not counted).
	maxSearchRequests = 10
	maxOtherTitles    = 2
	maxVariantTitles  = 2
	// closeScore is the distance below which results cannot be told apart: when the top scores differ by no more than this, details are fetched to disambiguate.
	closeScore = 0.1
	// variantCap is the score cap when only a variant title matched.
	variantCap = 0.95
	// segmentCap is the score cap when only one segment of a mixed Chinese-English title matched.
	segmentCap = 0.95

	// genreAnimation is TMDB's genre code for "Animation".
	genreAnimation = 16

	infoCacheTTL     = 30 * time.Minute
	infoCacheEntries = 256
	searchCacheTTL   = 30 * time.Minute
	searchCacheSize  = 256
)

// hintTitle is a tidied-up title hint.
type hintTitle struct {
	text   string
	role   pluginv1.TitleRole
	weight float64
}

// ---- Detail cache (aliases, translations, release dates, runtime, season/episode counts) ----

// titleInfo is a candidate's details (used for search scoring and absolute episode numbers).
type titleInfo struct {
	names        []string    // aliases and per-language translations
	releaseYears []int       // movies: release years in each country
	runtime      int         // movie runtime (minutes); 0 means unknown
	seasons      map[int]int // series: season number (>=1) -> episode count
	countries    []string    // production countries (ISO 3166-1)
}

func (i *titleInfo) totalEpisodes() int {
	n := 0
	if i != nil {
		for _, c := range i.seasons {
			n += c
		}
	}
	return n
}

type infoEntry struct {
	info    *titleInfo
	expires time.Time
}

// infoCache is the in-memory cache of candidate details (keyed by kind and ID; the content is independent of language); errors are not cached.
type infoCache struct {
	mu      sync.Mutex
	entries map[string]infoEntry
	group   singleflight.Group
}

func (ic *infoCache) peek(key string) (*titleInfo, bool) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	e, ok := ic.entries[key]
	if !ok || !time.Now().Before(e.expires) {
		return nil, false
	}
	return e.info, true
}

func (ic *infoCache) put(key string, info *titleInfo) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	if ic.entries == nil || len(ic.entries) >= infoCacheEntries {
		ic.entries = map[string]infoEntry{} // rarely full: when full, clear it all
	}
	ic.entries[key] = infoEntry{info: info, expires: time.Now().Add(infoCacheTTL)}
}

func infoKey(kind string, id int) string { return kind + "|" + strconv.Itoa(id) }

// info fetches a candidate's details (aliases, translations, release dates, runtime, season/episode counts); the same ID is not requested again within the TTL, and concurrent requests for the same ID are merged.
func (p *provider) info(ctx context.Context, c *tmdbClient, kind string, id int) (*titleInfo, error) {
	key := infoKey(kind, id)
	ic := &p.infos
	if info, ok := ic.peek(key); ok {
		return info, nil
	}
	v, err, _ := ic.group.Do(key, func() (any, error) {
		appendTo := "alternative_titles,translations"
		if kind == "movie" {
			appendTo += ",release_dates"
		}
		var it tmdbItem
		if err := c.get(ctx, "/"+kind+"/"+strconv.Itoa(id), url.Values{"append_to_response": {appendTo}}, &it); err != nil {
			return nil, err
		}
		info := &titleInfo{runtime: it.Runtime, countries: slices.Clone(it.OriginCountry)}
		for _, a := range it.AltTitles.all() {
			if t := strings.TrimSpace(a.Title); t != "" {
				info.names = append(info.names, t)
			}
		}
		for _, t := range it.translations() {
			if n := strings.TrimSpace(t.title()); n != "" {
				info.names = append(info.names, n)
			}
		}
		for _, c := range it.Countries {
			if c.ISO != "" && !slices.Contains(info.countries, c.ISO) {
				info.countries = append(info.countries, c.ISO)
			}
		}
		if it.ReleaseDates != nil {
			for _, r := range it.ReleaseDates.Results {
				for _, d := range r.Dates {
					if y := int(year(d.ReleaseDate)); y > 0 && !slices.Contains(info.releaseYears, y) {
						info.releaseYears = append(info.releaseYears, y)
					}
				}
			}
		}
		for _, s := range it.Seasons {
			if s.SeasonNumber >= 1 && s.EpisodeCount > 0 {
				if info.seasons == nil {
					info.seasons = map[int]int{}
				}
				info.seasons[s.SeasonNumber] = s.EpisodeCount
			}
		}
		ic.put(key, info)
		return info, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*titleInfo), nil
}

// ---- Search result cache ----

type searchEntry struct {
	results []tmdbSearchResult
	expires time.Time
}

// searchCache caches the results of name searches (the key includes kind, language, adult setting, year and query string); errors are not cached.
type searchCache struct {
	mu      sync.Mutex
	entries map[string]searchEntry
}

func (sc *searchCache) get(key string) ([]tmdbSearchResult, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	e, ok := sc.entries[key]
	if !ok || !time.Now().Before(e.expires) {
		return nil, false
	}
	return e.results, true
}

func (sc *searchCache) put(key string, r []tmdbSearchResult) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.entries == nil || len(sc.entries) >= searchCacheSize {
		sc.entries = map[string]searchEntry{}
	}
	sc.entries[key] = searchEntry{results: r, expires: time.Now().Add(searchCacheTTL)}
}

// ---- Search ----

// hintedSearch is the state of one hint search.
type hintedSearch struct {
	p      *provider
	c      *tmdbClient
	req    *pluginv1.SearchRequest
	kind   string
	lang   string
	adult  bool
	titles []hintTitle
	left   int // remaining requests
	hits   []tmdbSearchResult
	asked  map[string]bool
	infos  map[int]*titleInfo
}

// searchHinted searches and scores according to the query plan; it returns candidates in descending score order. If the first request fails it returns the error; if a later request fails (rate limiting excepted) it is skipped and the results already obtained are used.
func (p *provider) searchHinted(ctx context.Context, c *tmdbClient, adult bool, req *pluginv1.SearchRequest, kind, lang string) (*pluginv1.SearchResponse, error) {
	s := &hintedSearch{p: p, c: c, req: req, kind: kind, lang: lang, adult: adult, left: maxSearchRequests,
		asked: map[string]bool{}, infos: map[int]*titleInfo{}}
	s.titles = s.prepareTitles()
	if len(s.titles) == 0 {
		return &pluginv1.SearchResponse{}, nil
	}
	first := s.titles[0]
	year := 0
	if kind == "movie" && req.GetYear() > 0 && reliableYear(req.GetYearSource()) {
		year = int(req.GetYear())
	}
	if err := s.query(ctx, first.text, year, true); err != nil {
		return nil, err
	}
	step := func(texts []string, limit int) error {
		for _, t := range texts {
			if limit == 0 || s.best() >= stopScore {
				return nil
			}
			ok, err := s.queryOnce(ctx, t, 0)
			if err != nil {
				return err
			}
			if ok {
				limit--
			}
		}
		return nil
	}
	if year > 0 && s.best() < stopScore {
		if _, err := s.queryOnce(ctx, first.text, 0); err != nil { // 2: without the year
			return nil, err
		}
	}
	if err := step(s.otherTitles(), maxOtherTitles); err != nil { // 3: other titles
		return nil, err
	}
	if err := step(s.variantTitles(), maxVariantTitles); err != nil { // 4: variants
		return nil, err
	}
	if err := s.enrich(ctx); err != nil { // 5: aliases, translations, release dates, runtime, season/episode counts
		return nil, err
	}
	return s.response(), nil
}

func reliableYear(src pluginv1.YearSource) bool {
	return src == pluginv1.YearSource_YEAR_SOURCE_UNSPECIFIED || src == pluginv1.YearSource_YEAR_SOURCE_PAREN || src == pluginv1.YearSource_YEAR_SOURCE_TOKEN
}

// prepareTitles tidies the titles: in descending weight (equal weights keep the original order); name is always included and ranks before those of equal weight; of titles with identical text (case-insensitive) only the first is kept.
func (s *hintedSearch) prepareTitles() []hintTitle {
	var out []hintTitle
	seen := map[string]bool{}
	add := func(t hintTitle) {
		t.text = strings.TrimSpace(t.text)
		k := strings.ToLower(t.text)
		if t.text == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, t)
	}
	add(hintTitle{text: s.req.GetName(), role: pluginv1.TitleRole_TITLE_ROLE_PRIMARY, weight: 1})
	for _, t := range s.req.GetTitles() {
		add(hintTitle{text: t.GetText(), role: t.GetRole(), weight: t.GetWeight()})
	}
	// name is the first title (the current name when the user has renamed it): always first, the rest by weight.
	if len(out) > 1 {
		slices.SortStableFunc(out[1:], func(a, b hintTitle) int { return cmp.Compare(b.weight, a.weight) })
	}
	return out
}

// otherTitles are the titles of step 3: titles other than the first that are not variants.
func (s *hintedSearch) otherTitles() []string {
	var out []string
	for _, t := range s.titles[1:] {
		if t.role != pluginv1.TitleRole_TITLE_ROLE_VARIANT {
			out = append(out, t.text)
		}
	}
	return out
}

// variantTitles are the titles of step 4 (Chinese only): titles marked as variants, and when there are none, the other Chinese script of the first title is used as the variant.
func (s *hintedSearch) variantTitles() []string {
	if !hasHan(s.titles[0].text) {
		return nil
	}
	var out []string
	for _, t := range s.titles[1:] {
		if t.role == pluginv1.TitleRole_TITLE_ROLE_VARIANT {
			out = append(out, t.text)
		}
	}
	alt := s.p.fold.convert(s.titles[0].text, locale{lang: "zh", region: "TW"}, locale{lang: "zh", region: "CN"})
	if parseLocale(s.lang).script() == "Hans" {
		alt = s.p.fold.convert(s.titles[0].text, locale{lang: "zh", region: "CN"}, locale{lang: "zh", region: "TW"})
	}
	if alt != s.titles[0].text {
		out = append(out, alt)
	}
	return out
}

func (s *hintedSearch) askedKey(text string, year int) string {
	return strings.ToLower(text) + "|" + strconv.Itoa(year)
}

// queryOnce is like query, but one already asked (same text and year) is not asked again; it returns whether a query was really sent (or taken from the cache) this time.
func (s *hintedSearch) queryOnce(ctx context.Context, text string, year int) (bool, error) {
	if s.asked[s.askedKey(text, year)] {
		return false, nil
	}
	if s.left <= 0 {
		return false, nil
	}
	err := s.query(ctx, text, year, false)
	return err == nil, err
}

// query sends one name search and merges the results into the candidates (the same ID keeps only its first occurrence). When critical is true (the first query) a failure returns an error; other queries return an error only on rate limiting, token invalidation or
// a cancelled call, and ordinary failures are skipped.
func (s *hintedSearch) query(ctx context.Context, text string, year int, critical bool) error {
	s.asked[s.askedKey(text, year)] = true
	key := strings.Join([]string{s.kind, s.lang, strconv.FormatBool(s.adult), strconv.Itoa(year), text}, "\x00")
	results, ok := s.p.searches.get(key)
	if !ok {
		if s.left <= 0 {
			return nil
		}
		s.left--
		q := url.Values{"query": {text}, "language": {s.lang}, "include_adult": {strconv.FormatBool(s.adult)}}
		if year > 0 {
			q.Set("year", strconv.Itoa(year)) // affects ordering only
		}
		var page struct {
			Results []tmdbSearchResult `json:"results"`
		}
		if err := s.c.get(ctx, "/search/"+s.kind, q, &page); err != nil {
			return s.failure(ctx, err, critical, "search")
		}
		results = page.Results
		s.p.searches.put(key, results)
	}
	for _, h := range results {
		if !slices.ContainsFunc(s.hits, func(x tmdbSearchResult) bool { return x.ID == h.ID }) {
			s.hits = append(s.hits, h)
		}
	}
	return nil
}

// failure decides whether the failure of a later request aborts the whole search.
func (s *hintedSearch) failure(ctx context.Context, err error, critical bool, what string) error {
	if critical || ctx.Err() != nil || errors.Is(err, errUnauthorized) {
		return s.p.upstream(s.c, err, what)
	}
	if _, ok := errors.AsType[*sdk.RateLimitError](err); ok {
		return err // the SDK turns it into RESOURCE_EXHAUSTED with retry-after: the whole plugin pauses
	}
	s.c.log.Warn("TMDB hinted search: a request failed; continuing with what was found", slog.String("request", what), slog.String("error", err.Error()))
	return nil
}

// scored is one scored candidate.
type scored struct {
	h       tmdbSearchResult
	score   float64
	matched string
	reasons []string
}

func (s *hintedSearch) scoreAll() []scored {
	out := make([]scored, len(s.hits))
	for i, h := range s.hits {
		out[i] = s.p.hintScore(s.req, s.kind, s.titles, h, s.infos[h.ID])
	}
	return penalizeYearTwins(out)
}

// twinPenalty is the extra deduction for a candidate whose year is 1 to 2 years off when a same-title rival has an exact year match: works with the same name (for example two works with one name, two years apart)
// can be told apart only by year, and a 0.06 gap is not enough for the first to lead by 0.1; with only one candidate, or no rival whose year matches exactly, nothing changes.
const twinPenalty = 0.06

// penalizeYearTwins applies a further twinPenalty to candidates whose title is identical and whose year is only 1 to 2 years off, when the same result list holds a candidate whose title is identical and whose year matches exactly.
func penalizeYearTwins(list []scored) []scored {
	has := func(x scored, code string) bool { return slices.Contains(x.reasons, code) }
	exactTitle := func(x scored) bool { return has(x, "title.exact") || has(x, "title.alias") }
	anchored := false
	for _, x := range list {
		if exactTitle(x) && has(x, "year.exact") {
			anchored = true
			break
		}
	}
	if !anchored {
		return list
	}
	for i, x := range list {
		if exactTitle(x) && (has(x, "year.diff1") || has(x, "year.diff2")) {
			list[i].score = max(x.score-twinPenalty, 0)
			list[i].reasons = append(slices.Clone(x.reasons), "year.twin")
		}
	}
	return list
}

func (s *hintedSearch) best() float64 {
	best := 0.0
	for _, x := range s.scoreAll() {
		best = max(best, x.score)
	}
	return best
}

// enrich is step 5: when nothing matched (best score below goodMatch), or the top few cannot be told apart and there is information that can disambiguate (year, season/episode structure, runtime),
// it fetches the details of the top 3 and then rescores with aliases, release dates, season/episode counts and runtime.
func (s *hintedSearch) enrich(ctx context.Context) error {
	if len(s.hits) == 0 {
		return nil
	}
	ranked := s.scoreAll()
	slices.SortStableFunc(ranked, func(a, b scored) int { return cmp.Compare(b.score, a.score) })
	best := ranked[0].score
	need := best < goodMatch
	if !need && len(ranked) > 1 && ranked[1].score >= best-closeScore {
		need = s.req.GetShape() != nil || s.req.GetDurationSeconds() > 0 || s.req.GetYear() > 0
	}
	if !need {
		return nil
	}
	for i, x := range ranked {
		if i == aliasCandidates {
			break
		}
		if _, ok := s.infos[x.h.ID]; ok {
			continue
		}
		if _, cached := s.p.infos.peek(infoKey(s.kind, x.h.ID)); !cached {
			if s.left <= 0 {
				break
			}
			s.left--
		}
		info, err := s.p.info(ctx, s.c, s.kind, x.h.ID)
		if err != nil {
			if err := s.failure(ctx, err, false, "item"); err != nil {
				return err
			}
			continue
		}
		s.infos[x.h.ID] = info
	}
	return nil
}

// response sorts by descending score (ties by popularity, then by TMDB's order), applies limit and converts to the protocol.
func (s *hintedSearch) response() *pluginv1.SearchResponse {
	list := s.scoreAll()
	slices.SortStableFunc(list, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(b.h.Popularity, a.h.Popularity)
	})
	if n := int(s.req.GetLimit()); n > 0 && len(list) > n {
		list = list[:n]
	}
	resp := &pluginv1.SearchResponse{}
	for _, x := range list {
		c := candidate(x.h, s.req.GetType(), s.p.imageSizes().poster)
		c.Score, c.MatchedTitle, c.Reasons = x.score, x.matched, x.reasons
		resp.Candidates = append(resp.Candidates, c)
	}
	return resp
}

// ---- Scoring ----

// hintScore returns the candidate's score (0 to 1), the name that matched and the reason codes. Features (research document 5.2):
//   - Title: for each title hint, the highest score over the candidate's localized name, original name, aliases and translations (sequel numbers normalized; the segments of a mixed Chinese-English title are compared separately, capped at segmentCap;
//     variants capped at variantCap).
//   - Year: graded by year source and title score; a release year in any country equal to the local year counts as a difference of 0.
//   - Shape (series): +0.03 when the local seasons and per-season episode counts all fit, -0.1 on a clear mismatch; the absolute episode count of one season fitting the whole series also counts as fitting.
//   - Anime flag: +0.02 when the candidate has the animation genre, -0.05 when not.
//   - Language and region: +0.03 when the original language or a production country matches the hint, no deduction when there is no data.
//   - Runtime (movies, when details were fetched): within 10% +0.03, over 30% off -0.05.
//   - Missing poster -0.01; the sequel guard for movies is the same as in search.go. Popularity is used only for ordering (on ties).
func (p *provider) hintScore(req *pluginv1.SearchRequest, kind string, titles []hintTitle, h tmdbSearchResult, info *titleInfo) scored {
	type name struct {
		text  string
		alias bool
	}
	names := []name{{first(h.Title, h.Name), false}, {first(h.OriginalTitle, h.OriginalName), false}}
	if info != nil {
		for _, n := range info.names {
			names = append(names, name{n, true})
		}
	}
	var best float64
	var matched string
	var alias, capped bool
	var bestHint hintTitle
	for _, t := range titles {
		for _, n := range names {
			if n.text == "" {
				continue
			}
			s, c := p.hintTitleScore(t, n.text)
			if s > best {
				best, matched, alias, capped, bestHint = s, n.text, n.alias, c, t
			}
		}
	}
	var reasons []string
	switch {
	case best >= 0.999 && alias:
		reasons = append(reasons, "title.alias")
	case best >= 0.999:
		reasons = append(reasons, "title.exact")
	case best >= 0.85:
		reasons = append(reasons, "title.prefix")
	case best >= 0.6:
		reasons = append(reasons, "title.partial")
	default:
		reasons = append(reasons, "title.weak")
	}
	if capped {
		reasons = append(reasons, "title.variant_cap")
	}
	s := best
	strong := best >= 0.999
	var rd []int
	if info != nil {
		rd = info.releaseYears
	}
	d, r := yearAdjust(int(req.GetYear()), req.GetYearSource(), int(year(first(h.ReleaseDate, h.FirstAirDate))), rd, strong)
	s += d
	reasons = append(reasons, r)
	yearReason := r
	if kind == "tv" {
		if d, r := shapeAdjust(req.GetShape(), info); r != "" {
			s += d
			reasons = append(reasons, r)
		}
	}
	if slices.Contains(req.GetFlags(), "anime") && len(h.GenreIDs) > 0 {
		if slices.Contains(h.GenreIDs, genreAnimation) {
			s += 0.02
			reasons = append(reasons, "genre.anime")
		} else {
			s -= 0.05
			reasons = append(reasons, "genre.not_anime")
		}
	}
	if r := regionMatch(req, h, info); r != "" {
		s += 0.03
		reasons = append(reasons, r)
	}
	if kind == "movie" && info != nil && info.runtime > 0 && req.GetDurationSeconds() > 0 {
		have, want := float64(info.runtime)*60, float64(req.GetDurationSeconds())
		switch diff := abs(have-want) / want; {
		case diff <= 0.1:
			s += 0.03
			reasons = append(reasons, "runtime.ok")
		case diff > 0.3:
			s -= 0.05
			reasons = append(reasons, "runtime.off")
		}
	}
	if h.PosterPath == "" {
		s -= 0.01
	}
	if kind == "movie" {
		// Sequel guard: when the query carries a sequel number and the candidate has no such number, the score is held below the auto-apply threshold (see sequel.go).
		all := []string{first(h.Title, h.Name), first(h.OriginalTitle, h.OriginalName)}
		if info != nil {
			all = append(all, info.names...)
		}
		// Exception: the matched title is itself identical (not one capped by the variant cap), the year also matches exactly, and it has no number: the number appears only in another title (for example a
		// Chinese name with the sequel number 2 paired with the English name "Top Gun Maverick" matching Top Gun: Maverick). The first movie of a series cannot have the same name and the same year at once,
		// so the guard's goal (an identical-titled first movie getting a full score) does not apply; when the year is unknown or does not match, the guard stays as before.
		exempt := best >= 0.999 && !capped && yearReason == "year.exact" && sequelNumber(bestHint.text) == 0
		if g := sequelGuard(titles[0].text, all, s); g < s && !exempt {
			s = g
			reasons = append(reasons, "sequel.cap")
		} else if g < s {
			reasons = append(reasons, "sequel.exempt")
		}
	}
	return scored{h: h, score: min(max(s, 0), 1), matched: matched, reasons: reasons}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// hintTitleScore compares one title hint with one candidate name: the base score is the higher of the original text and the text after sequel number normalization; a mixed Chinese-English title is split into segments and each is compared;
// a variant hint is capped at variantCap. The second return value says the score was held down by the variant cap.
func (p *provider) hintTitleScore(t hintTitle, name string) (float64, bool) {
	s := p.titleScore(t.text, name)
	if ft, fn := foldSequel(t.text), foldSequel(name); ft != t.text || fn != name {
		s = max(s, p.titleScore(ft, fn))
	}
	// Identical only after removing edition words (theatrical version, movie version, The Movie): nearly identical, capped at editionCap.
	qs, qOK := stripFormatWords(t.text)
	ns, nOK := stripFormatWords(name)
	capped := false
	if qOK || nOK {
		if e := p.titleScore(qs, ns); e > s {
			s = min(e, editionCap)
			capped = e > editionCap
		}
	}
	if hasHan(t.text) && hasLatin(t.text) {
		for _, seg := range scriptSegments(t.text) {
			if len([]rune(p.normalize(seg))) >= 2 {
				s = max(s, min(p.titleScore(seg, name), segmentCap))
			}
		}
	}
	if t.role == pluginv1.TitleRole_TITLE_ROLE_VARIANT && s > variantCap {
		return variantCap, true
	}
	return s, capped
}

func hasLatin(s string) bool {
	for _, r := range s {
		if r < unicode.MaxLatin1 && unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// scriptSegments splits a mixed Chinese-English title into Han segments and Latin segments (digits and punctuation go with the preceding segment).
func scriptSegments(s string) []string {
	var segs []string
	var cur []rune
	curHan := false
	flush := func() {
		if t := strings.TrimSpace(string(cur)); t != "" {
			segs = append(segs, t)
		}
		cur = cur[:0]
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			if !curHan {
				flush()
			}
			curHan = true
		case unicode.IsLetter(r):
			if curHan {
				flush()
			}
			curHan = false
		}
		cur = append(cur, r)
	}
	flush()
	return segs
}

var (
	reRomanToken   = regexp.MustCompile(`(?i)\b(II|III|IV|VI|VII|VIII|IX|XI|XII)\b`)
	reCNOrdinal    = regexp.MustCompile(`第\s*([一二三四五六七八九十两兩]{1,3})\s*[部集章篇季]`)
	reCNTrailDigit = regexp.MustCompile(`(\p{Han})([二三四五六七八九十])$`)
)

// foldSequel unifies sequel number notations into Arabic digits: `Ⅱ`/`II` -> 2, the Chinese "part two" form -> 2, a trailing Chinese numeral two -> 2 (for matching only; the query string is not changed).
func foldSequel(s string) string {
	s = norm.NFKC.String(s)
	s = reRomanToken.ReplaceAllStringFunc(s, func(m string) string { return strconv.Itoa(romanValue[strings.ToUpper(m)]) })
	s = reCNOrdinal.ReplaceAllStringFunc(s, func(m string) string {
		sub := reCNOrdinal.FindStringSubmatch(m)
		return strconv.Itoa(cnNumber(sub[1]))
	})
	s = reCNTrailDigit.ReplaceAllStringFunc(s, func(m string) string {
		sub := reCNTrailDigit.FindStringSubmatch(m)
		return sub[1] + strconv.Itoa(cnNumber(sub[2]))
	})
	return s
}

// cnNumber parses one to three Han numerals (one to ten, eleven to nineteen, and the simple forms of twenty to ninety-nine).
func cnNumber(s string) int {
	n := 0
	for _, r := range s {
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
	return n
}

// yearAdjust returns the year's bonus or deduction and the reason. No deduction when want is 0 or the year source is NONE (but a candidate without a year still has a small deduction with the reason code year.unknown).
// strong means the title is identical or an alias matched: a 2-year difference then costs only 0.06, otherwise 0.15. When the year was inferred from context (CONTEXT) the deduction is halved.
func yearAdjust(want int, src pluginv1.YearSource, y int, releaseYears []int, strong bool) (float64, string) {
	if want <= 0 || src == pluginv1.YearSource_YEAR_SOURCE_NONE {
		return 0, "year.unknown"
	}
	if y == 0 && len(releaseYears) == 0 {
		return -0.1, "year.missing"
	}
	diff, reason := abs(float64(y-want)), ""
	if slices.Contains(releaseYears, want) {
		if y != want {
			reason = "year.release_date"
		}
		diff = 0
	}
	var d float64
	switch {
	case diff == 0:
		if reason == "" {
			reason = "year.exact"
		}
	case diff == 1:
		d, reason = -0.03, "year.diff1"
	case diff == 2 && strong:
		d, reason = -0.06, "year.diff2"
	case diff == 2:
		d, reason = -0.15, "year.diff2"
	default:
		d, reason = -0.3, "year.far"
	}
	if src == pluginv1.YearSource_YEAR_SOURCE_CONTEXT {
		d /= 2
	}
	return d, reason
}

// shapeAdjust compares the local season/episode structure with the candidate's season/episode counts (series; an empty reason when there are no details or no structure).
func shapeAdjust(shape *pluginv1.SeriesShape, info *titleInfo) (float64, string) {
	if shape == nil || info == nil || len(info.seasons) == 0 {
		return 0, ""
	}
	total := info.totalEpisodes()
	maxSeason := 0
	for s := range info.seasons {
		maxSeason = max(maxSeason, s)
	}
	localSeasons := 0
	mismatch := false
	for _, s := range shape.GetSeasonNumbers() {
		if s < 1 {
			continue
		}
		localSeasons++
		if int(s) > maxSeason {
			mismatch = true
		}
	}
	for s, n := range shape.GetEpisodesPerSeason() {
		if s < 1 {
			continue
		}
		cnt, known := info.seasons[int(s)]
		if !known || int(n) <= cnt+2 {
			continue
		}
		// Consecutive numbering for anime: only one season, with more episodes than TMDB's season, but fitting the whole series, still counts as fitting.
		if localSeasons <= 1 && int(n) <= total+2 {
			continue
		}
		mismatch = true
	}
	if int(shape.GetMaxEpisodeNumber()) > total+2 {
		mismatch = true
	}
	if mismatch {
		return -0.1, "shape.mismatch"
	}
	if localSeasons == 0 && shape.GetMaxEpisodeNumber() == 0 {
		return 0, ""
	}
	return 0.03, "shape.ok"
}

// regionMatch reports whether the candidate's original language or production country matches the hint (empty when there is no data).
func regionMatch(req *pluginv1.SearchRequest, h tmdbSearchResult, info *titleInfo) string {
	if l := originalLanguage(h.OriginalLang); l != "" {
		for _, want := range req.GetLanguageHints() {
			if strings.EqualFold(want, l) {
				return "language.match"
			}
		}
	}
	countries := slices.Clone(h.OriginCountry)
	if info != nil {
		countries = append(countries, info.countries...)
	}
	for _, c := range countries {
		for _, want := range req.GetRegionHints() {
			if strings.EqualFold(want, c) {
				return "region.match"
			}
		}
	}
	return ""
}

// ---- Absolute episode numbers ----

// resolveAbsolute maps an anime's absolute episode number back to (season, episode): by accumulating the episode counts of the series' seasons (from season 1, specials excluded). The season/episode counts of the series come from info (cached).
func (p *provider) resolveAbsolute(ctx context.Context, c *tmdbClient, series string, abs int) (season, episode int, err error) {
	id, convErr := strconv.Atoi(series)
	if convErr != nil || id <= 0 {
		return 0, 0, invalidArgument("series_id is not a TMDB id")
	}
	info, ierr := p.info(ctx, c, "tv", id)
	if ierr != nil {
		return 0, 0, p.upstream(c, ierr, "series")
	}
	seasons := make([]int, 0, len(info.seasons))
	for s := range info.seasons {
		seasons = append(seasons, s)
	}
	slices.Sort(seasons)
	n := abs
	for _, s := range seasons {
		if n <= info.seasons[s] {
			return s, n, nil
		}
		n -= info.seasons[s]
	}
	return 0, 0, p.upstream(c, notFoundEpisode(series, 0, abs), "episode")
}
