package main

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Seasons and episodes are taken from the whole-season response: /tv/{id}/season/{n} already contains every episode's name, overview, still and crew and guest stars in that language.
// A refresh of a 20-episode season therefore drops from 20-40 requests to as many requests as the fallback chain is long; results are cached in the plugin by (series, season, language).
// This complies with the TMDB terms' "cache no longer than 6 months".

const (
	seasonCacheTTL     = 30 * time.Minute
	seasonCacheEntries = 64
)

type tmdbEpisode struct {
	ID            int        `json:"id"`
	Name          string     `json:"name"`
	Overview      string     `json:"overview"`
	AirDate       string     `json:"air_date"`
	EpisodeNumber int        `json:"episode_number"`
	SeasonNumber  int        `json:"season_number"`
	Runtime       int        `json:"runtime"`
	StillPath     string     `json:"still_path"`
	VoteAverage   float64    `json:"vote_average"`
	VoteCount     int64      `json:"vote_count"`
	Crew          []tmdbCrew `json:"crew"`
	GuestStars    []tmdbCast `json:"guest_stars"`
}

type tmdbSeason struct {
	ID           int               `json:"id"`
	Name         string            `json:"name"`
	Overview     string            `json:"overview"`
	AirDate      string            `json:"air_date"`
	SeasonNumber int               `json:"season_number"`
	PosterPath   string            `json:"poster_path"`
	Episodes     []tmdbEpisode     `json:"episodes"`
	Images       *tmdbImages       `json:"images"`
	Translations *tmdbTranslations `json:"translations"`
	Credits      *tmdbCredits      `json:"credits"`
}

func (s *tmdbSeason) episode(n int) (tmdbEpisode, bool) {
	for _, e := range s.Episodes {
		if e.EpisodeNumber == n {
			return e, true
		}
	}
	return tmdbEpisode{}, false
}

// seasonCache is the in-memory cache of whole-season responses; errors are not cached.
type seasonCache struct {
	mu      sync.Mutex
	entries map[string]seasonEntry
	names   map[string]namesEntry // series names (see seriesNames), keyed by the series ID
	group   singleflight.Group
}

type namesEntry struct {
	names   map[string]bool
	expires time.Time
}

type seasonEntry struct {
	season  *tmdbSeason
	expires time.Time
}

// season fetches the whole-season response for (series, season, language). When full is true it also brings the season's images, translations and credits (for the season itself and the target language);
// fallback languages only need episode names and overviews, so they do not. Concurrent requests for the same key are merged into one.
func (p *provider) season(ctx context.Context, c *tmdbClient, series string, n int, lang locale, imageLangs string, full bool) (*tmdbSeason, error) {
	key := fmt.Sprintf("%s|%d|%s|%v|%s", series, n, lang, full, imageLangs)
	sc := &p.seasons
	sc.mu.Lock()
	if e, ok := sc.entries[key]; ok && time.Now().Before(e.expires) {
		sc.mu.Unlock()
		return e.season, nil
	}
	sc.mu.Unlock()
	v, err, _ := sc.group.Do(key, func() (any, error) {
		q := url.Values{"language": {lang.String()}}
		if full {
			q.Set("append_to_response", "images,translations,credits")
			q.Set("include_image_language", imageLangs)
		}
		var s tmdbSeason
		if err := c.get(ctx, fmt.Sprintf("/tv/%s/season/%d", url.PathEscape(series), n), q, &s); err != nil {
			return nil, err
		}
		now := time.Now()
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if sc.entries == nil {
			sc.entries = map[string]seasonEntry{}
		}
		if len(sc.entries) >= seasonCacheEntries {
			for k, e := range sc.entries { // clear the expired ones first; if still full, clear the one that expires earliest
				if now.After(e.expires) {
					delete(sc.entries, k)
				}
			}
			for len(sc.entries) >= seasonCacheEntries {
				oldest := ""
				for k, e := range sc.entries {
					if oldest == "" || e.expires.Before(sc.entries[oldest].expires) {
						oldest = k
					}
				}
				delete(sc.entries, oldest)
			}
		}
		sc.entries[key] = seasonEntry{season: &s, expires: now.Add(seasonCacheTTL)}
		return &s, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*tmdbSeason), nil
}

func (p *provider) getSeason(ctx context.Context, c *tmdbClient, seriesID string, n int, ch []locale) (*pluginv1.ItemMetadata, error) {
	target := ch[0]
	s, err := p.season(ctx, c, seriesID, n, target, imageLanguages(ch), true)
	if err != nil {
		return nil, p.upstream(c, err, "season")
	}
	var ts []tmdbTranslation
	if s.Translations != nil {
		ts = s.Translations.Translations
	}
	idx := int32(n)
	m := &pluginv1.ItemMetadata{Type: pluginv1.ItemType_ITEM_TYPE_SEASON, IndexNumber: &idx, PremiereDate: s.AirDate,
		ProviderIds: map[string]string{}}
	m.ProductionYear = year(m.PremiereDate)
	// Season names almost never have translations (placeholders such as "Season 1" and its Chinese equivalent), and some are wrongly filled in with the series name: use the season name only if there is a real one in the same language,
	// otherwise generate one from the target language.
	series := p.seriesNames(ctx, c, seriesID)
	usable := func(v string) bool { return realName(v) && !series[p.normalize(v)] }
	if v, ok := pick(ts, sameLanguage(ch), func(t tmdbTranslation) string { return t.Data.Name }, usable); ok {
		m.Name, m.Language = p.localize(v, target), p.outLang(v.from, target)
	} else {
		m.Name, m.Language = seasonName(target, n), target.String() // the generated name is in the target language
	}
	if v, ok := pick(ts, ch, func(t tmdbTranslation) string { return t.Data.Overview }, nonEmpty); ok {
		m.Overview = p.localize(v, target)
	}
	var imgs tmdbImages
	if s.Images != nil {
		imgs = *s.Images
	}
	m.Images = convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, imgs.Posters, s.PosterPath, ch)
	m.Credits = credits(s.Credits, nil, p.castLimit())
	return m, nil
}

// seriesNames returns the series' names in each language (normalized, see normalize), used to rule out season names wrongly filled in with the series name.
// Returns nil when it cannot be fetched (the season's other fields are unaffected).
func (p *provider) seriesNames(ctx context.Context, c *tmdbClient, series string) map[string]bool {
	sc := &p.seasons
	sc.mu.Lock()
	if e, ok := sc.names[series]; ok && time.Now().Before(e.expires) {
		sc.mu.Unlock()
		return e.names
	}
	sc.mu.Unlock()
	var ts tmdbTranslations
	if err := c.get(ctx, "/tv/"+url.PathEscape(series)+"/translations", nil, &ts); err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, t := range ts.Translations {
		if n := p.normalize(t.title()); n != "" {
			names[n] = true
		}
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.names == nil || len(sc.names) >= seasonCacheEntries {
		sc.names = map[string]namesEntry{} // rarely used: when full, clear everything
	}
	sc.names[series] = namesEntry{names: names, expires: time.Now().Add(seasonCacheTTL)}
	return names
}

func (p *provider) getEpisode(ctx context.Context, c *tmdbClient, series string, sn, en int, ch []locale) (*pluginv1.ItemMetadata, error) {
	target := ch[0]
	s, err := p.season(ctx, c, series, sn, target, imageLanguages(ch), true)
	if err != nil {
		return nil, p.upstream(c, err, "episode")
	}
	e, ok := s.episode(en)
	if !ok {
		return nil, p.upstream(c, notFoundEpisode(series, sn, en), "episode")
	}
	si, ei := int32(sn), int32(en)
	m := &pluginv1.ItemMetadata{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, IndexNumber: &ei, ParentIndexNumber: &si,
		PremiereDate: e.AirDate, CommunityRating: e.VoteAverage, Ratings: tmdbRatings(e.VoteAverage, e.VoteCount), RuntimeSeconds: int64(e.Runtime) * 60,
		ProviderIds: map[string]string{}}
	m.ProductionYear = year(m.PremiereDate)
	// The target language's episode names and overviews; when missing (empty string, placeholder, or for a Chinese target no Han characters), fill in from the whole-season response of each fallback in the chain, one request each.
	name, overview := e.Name, e.Overview
	if !episodeName(name, target) {
		name = ""
	}
	nameLang, overviewLang := target.String(), target.String() // actual language: the episode name takes priority; when there is no episode name, look at the overview
	if name == "" {
		nameLang = ""
	}
	if overview == "" {
		overviewLang = ""
	}
	for _, l := range ch[1:] {
		if name != "" && overview != "" {
			break
		}
		fb, err := p.season(ctx, c, series, sn, l, "", false)
		if err != nil {
			continue // a fallback language failing does not affect the main result
		}
		fe, ok := fb.episode(en)
		if !ok {
			continue
		}
		if name == "" && episodeName(fe.Name, l) {
			name, nameLang = p.conv.convert(fe.Name, l, target), p.outLang(l, target)
		}
		if overview == "" && fe.Overview != "" {
			overview, overviewLang = p.conv.convert(fe.Overview, l, target), p.outLang(l, target)
		}
	}
	m.Name, m.Overview, m.Language = name, overview, first(nameLang, overviewLang)
	// when all episode names are placeholders leave it empty, and the server or client shows "Episode N"
	m.Images = convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, nil, e.StillPath, ch) // an episode's primary image is its still
	m.Credits = credits(&tmdbCredits{Crew: e.Crew}, e.GuestStars, p.castLimit())
	return m, nil
}

// episodeName reports whether an episode name is a real name in that language: non-empty, not a placeholder; for Chinese it must contain Han characters
// (when a translation is missing TMDB occasionally returns the original-language name).
func episodeName(s string, l locale) bool {
	return realName(s) && (l.lang != "zh" || hasHan(s))
}
