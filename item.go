package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// GetItem implements pluginv1.MetadataProviderServer. For movies and series one request brings back details, credits, images, external IDs, certifications and
// translations; seasons and episodes are taken from the whole-season response (see season.go). Text is chosen along the fallback chain and converted to the target language (see lang.go, text.go).
func (p *provider) GetItem(ctx context.Context, req *pluginv1.GetItemRequest) (*pluginv1.ItemMetadata, error) {
	c, _, err := p.ready()
	if err != nil {
		return nil, err
	}
	ch := chain(req.GetLanguages())
	switch req.GetType() {
	case pluginv1.ItemType_ITEM_TYPE_MOVIE, pluginv1.ItemType_ITEM_TYPE_SERIES:
		return p.getTitle(ctx, c, req, ch)
	case pluginv1.ItemType_ITEM_TYPE_SEASON:
		if req.GetSeriesId() == "" || req.SeasonNumber == nil {
			return nil, invalidArgument("season needs series_id and season_number")
		}
		return p.getSeason(ctx, c, req.GetSeriesId(), int(req.GetSeasonNumber()), ch)
	case pluginv1.ItemType_ITEM_TYPE_EPISODE:
		if req.AbsoluteEpisodeNumber != nil && req.SeasonNumber == nil && req.EpisodeNumber == nil {
			// Absolute episode numbers for anime: mapped back to (season, episode) using each season's episode count.
			if req.GetSeriesId() == "" || req.GetAbsoluteEpisodeNumber() < 1 {
				return nil, invalidArgument("an absolute episode needs series_id and a number of at least 1")
			}
			sn, en, err := p.resolveAbsolute(ctx, c, req.GetSeriesId(), int(req.GetAbsoluteEpisodeNumber()))
			if err != nil {
				return nil, err
			}
			return p.getEpisode(ctx, c, req.GetSeriesId(), sn, en, ch)
		}
		if req.GetSeriesId() == "" || req.SeasonNumber == nil || req.EpisodeNumber == nil {
			return nil, invalidArgument("episode needs series_id, season_number and episode_number")
		}
		return p.getEpisode(ctx, c, req.GetSeriesId(), int(req.GetSeasonNumber()), int(req.GetEpisodeNumber()), ch)
	}
	return nil, invalidArgument("unsupported item type")
}

func (p *provider) getTitle(ctx context.Context, c *tmdbClient, req *pluginv1.GetItemRequest, ch []locale) (*pluginv1.ItemMetadata, error) {
	if req.GetId() == "" {
		return nil, invalidArgument("id is required")
	}
	movie := req.GetType() == pluginv1.ItemType_ITEM_TYPE_MOVIE
	path := "/tv/" + url.PathEscape(req.GetId())
	// /tv/{id}'s credits only has the cast of the latest season; aggregate_credits covers all seasons.
	appendTo := "aggregate_credits,images,external_ids,content_ratings,translations,alternative_titles,videos"
	if movie {
		path = "/movie/" + url.PathEscape(req.GetId())
		appendTo = "credits,images,external_ids,release_dates,translations,alternative_titles,videos"
	}
	var it tmdbItem
	q := url.Values{"language": {ch[0].String()}, "append_to_response": {appendTo}, "include_image_language": {imageLanguages(ch)},
		"include_video_language": {videoLanguages(ch)}}
	if err := c.get(ctx, path, q, &it); err != nil {
		return nil, p.upstream(c, err, "item")
	}
	var g gaps
	g.add(p.addOriginalLanguageImages(ctx, c, path, &it, ch))
	m := p.titleMetadata(req.GetType(), it, ch)
	g.apply(func(n []*pluginv1.Notice, incomplete bool, retry uint32) {
		m.Notices, m.Incomplete, m.RetryAfterSeconds = n, incomplete, retry
	})
	return m, nil
}

// addOriginalLanguageImages adds images in the work's original language (the image-language preference can only rank candidates in the original language if they exist).
// include_image_language only carries the languages on the fallback chain plus xx and null; the original language is only known once the details come back, so when it is not on the chain one more
// /images request is sent asking only for that language (not needed when the chain has it). A failure of this request does not affect the main result (the original-language images are just not added); the caller reports it as a gap so the host asks again later.
func (p *provider) addOriginalLanguageImages(ctx context.Context, c *tmdbClient, path string, it *tmdbItem, ch []locale) error {
	orig := originalLanguage(it.OriginalLang)
	if orig == "" || slices.ContainsFunc(ch, func(l locale) bool { return l.lang == orig }) {
		return nil
	}
	var extra tmdbImages
	if err := c.get(ctx, path+"/images", url.Values{"include_image_language": {orig}}, &extra); err != nil {
		return err
	}
	if it.Images == nil {
		it.Images = &tmdbImages{}
	}
	merge := func(dst *[]tmdbImage, add []tmdbImage) {
		for _, i := range add {
			if i.Lang == orig && !slices.ContainsFunc(*dst, func(x tmdbImage) bool { return x.FilePath == i.FilePath }) {
				*dst = append(*dst, i)
			}
		}
	}
	merge(&it.Images.Posters, extra.Posters)
	merge(&it.Images.Backdrops, extra.Backdrops)
	merge(&it.Images.Logos, extra.Logos)
	return nil
}

// originalLanguage normalizes TMDB's original_language to BCP 47: "xx" (no linguistic content) counts as unknown, and "cn" (TMDB's non-standard code for Cantonese) counts as zh.
func originalLanguage(l string) string {
	switch l = strings.ToLower(strings.TrimSpace(l)); l {
	case "xx":
		return ""
	case "cn":
		return "zh"
	}
	return l
}

// titleMetadata converts a movie's or series' details to the protocol format.
func (p *provider) titleMetadata(t pluginv1.ItemType, it tmdbItem, ch []locale) *pluginv1.ItemMetadata {
	sz := p.imageSizes()
	target := ch[0]
	ts := it.translations()
	original := first(it.OriginalTitle, it.OriginalName)
	name, lang := p.pickTitle(ts, it.AltTitles.all(), ch, original, it.OriginalLang)
	m := &pluginv1.ItemMetadata{
		Id: strconv.Itoa(it.ID), Type: t, Name: name, Language: lang, OriginalTitle: original, OriginalLanguage: originalLanguage(it.OriginalLang),
		CommunityRating: it.VoteAverage, Ratings: tmdbRatings(it.VoteAverage, it.VoteCount), ProviderIds: map[string]string{"tmdb": strconv.Itoa(it.ID)},
		Status: seriesStatus(t, it.Status),
	}
	if v, ok := pick(ts, ch, func(t tmdbTranslation) string { return t.Data.Overview }, nonEmpty); ok {
		m.Overview = p.localize(v, target)
	}
	if v, ok := pick(ts, ch, func(t tmdbTranslation) string { return t.Data.Tagline }, nonEmpty); ok {
		m.Tagline = p.localize(v, target)
	}
	if len(ts) == 0 { // no translations (should not happen): use the details' own text
		m.Name, m.Overview, m.Tagline = first(it.Title, it.Name, original), it.Overview, it.Tagline
	}
	m.PremiereDate = first(it.ReleaseDate, it.FirstAirDate)
	m.ProductionYear = year(m.PremiereDate)
	if t == pluginv1.ItemType_ITEM_TYPE_SERIES && (it.Status == "Ended" || it.Status == "Canceled") {
		m.EndDate = it.LastAirDate // for a series still airing, last_air_date is only the date of the latest episode
	}
	switch {
	case it.Runtime > 0:
		m.RuntimeSeconds = int64(it.Runtime) * 60
	case len(it.EpisodeRunTime) > 0:
		m.RuntimeSeconds = int64(it.EpisodeRunTime[0]) * 60
	}
	for _, g := range it.Genres {
		m.Genres = append(m.Genres, p.genreName(g.ID, g.Name, target))
	}
	for _, s := range append(slices.Clone(it.Networks), it.Companies...) {
		if !slices.Contains(m.Studios, s.Name) {
			m.Studios = append(m.Studios, s.Name)
		}
	}
	for _, ctry := range it.Countries {
		m.ProductionLocations = append(m.ProductionLocations, ctry.Name)
	}
	if ids := it.ExternalIDs; ids != nil {
		if ids.IMDb != "" {
			m.ProviderIds["imdb"] = ids.IMDb
		}
		if ids.TVDB > 0 {
			m.ProviderIds["tvdb"] = strconv.Itoa(ids.TVDB)
		}
	}
	if it.IMDbID != "" {
		m.ProviderIds["imdb"] = it.IMDbID
	}
	m.OfficialRating = officialRating(it, p.ratingCountry(target))
	if col := it.Collection; col != nil && col.ID > 0 {
		m.Collection = &pluginv1.CollectionRef{Id: strconv.Itoa(col.ID), Name: p.collectionName(col.Name)}
	}
	var imgs tmdbImages
	if it.Images != nil {
		imgs = *it.Images
	}
	m.Images = append(m.Images, convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, imgs.Posters, it.PosterPath, ch, sz.poster)...)
	m.Images = append(m.Images, capImages(convertImages(pluginv1.ImageType_IMAGE_TYPE_BACKDROP, imgs.Backdrops, it.BackdropPath, ch, sz.backdrop), p.backdropLimit())...)
	m.Images = append(m.Images, convertImages(pluginv1.ImageType_IMAGE_TYPE_LOGO, imgs.Logos, "", ch, sizeOriginal)...)
	cr := it.Credits
	if it.Aggregate != nil {
		cr = it.Aggregate
	}
	m.Credits = credits(cr, nil, p.castLimit(), sz.profile)
	m.Trailers = trailers(it.Videos, ch)
	return m
}

// pickTitle picks the title and also returns its actual language (ItemMetadata.language): the target language's translated name →
// (Chinese) a Chinese alias for the target region → the original name when it is in the target language → a translated name in another language on the fallback chain (converted to the target script for Chinese) → the original name.
// TMDB's aliases are often more complete than its translations (for example a Taiwan title is registered only under TW in alternative_titles); a Japanese film in a Japanese library should not become an English title.
// Language: Chinese converted to the target script counts as the target language; falling back to a translated name in another language is that language; falling back to the original name is the original language (TMDB gives only a two-letter code).
func (p *provider) pickTitle(ts []tmdbTranslation, alts []tmdbAltTitle, ch []locale, original, originalLang string) (string, string) {
	target := ch[0]
	title := func(t tmdbTranslation) string { return t.title() }
	if v, ok := pick(ts, ch[:1], title, nonEmpty); ok {
		return v.text, target.String()
	}
	if target.lang == "zh" {
		for _, a := range alts {
			if a.Region == target.region && hasHan(a.Title) {
				return a.Title, target.String()
			}
		}
	}
	if originalLang == target.lang && target.lang != "zh" && original != "" {
		return original, target.String()
	}
	if v, ok := pick(ts, ch[1:], title, nonEmpty); ok {
		return p.localize(v, target), p.outLang(v.from, target)
	}
	return original, originalLang
}

// outLang returns the language, at output time, of text taken from from: Simplified/Traditional converted to the target script (when conversion is on) counts as the target language, otherwise it is the source language.
func (p *provider) outLang(from, target locale) string {
	if from.lang == "zh" && target.lang == "zh" && !p.conv.disabled() {
		return target.String()
	}
	return from.String()
}

// seriesStatus maps TMDB's series status to the protocol: Returning Series, In Production, Planned and Pilot are continuing,
// Ended and Canceled are ended, and anything else, as well as movies, is left unset.
func seriesStatus(t pluginv1.ItemType, s string) pluginv1.SeriesStatus {
	if t != pluginv1.ItemType_ITEM_TYPE_SERIES {
		return pluginv1.SeriesStatus_SERIES_STATUS_UNSPECIFIED
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "returning series", "in production", "planned", "pilot":
		return pluginv1.SeriesStatus_SERIES_STATUS_CONTINUING
	case "ended", "canceled", "cancelled":
		return pluginv1.SeriesStatus_SERIES_STATUS_ENDED
	}
	return pluginv1.SeriesStatus_SERIES_STATUS_UNSPECIFIED
}

// ratingCountry is the country used to pick the certification: the certification_country setting, otherwise the target language's region, otherwise the US.
func (p *provider) ratingCountry(target locale) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return first(p.certCountry, target.region, "US")
}

// backdropLimit is the cap on the number of backdrops (0 means all).
func (p *provider) backdropLimit() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxBackdrops
}

// imageSizes returns the configured download sizes.
func (p *provider) imageSizes() imageSizes {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sizes
}

// capImages keeps only the first n (0 means all); the order is the one convertImages arranged (language priority chain, rating, resolution), so what is kept is the best n.
func capImages(in []*pluginv1.Image, n int) []*pluginv1.Image {
	if n > 0 && len(in) > n {
		return in[:n]
	}
	return in
}

func (p *provider) castLimit() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxCast
}

func notFoundEpisode(series string, s, e int) error {
	return fmt.Errorf("episode S%02dE%02d of series %s: %w", s, e, series, errNotFound)
}

// tmdbRatings is TMDB's own rating: source tmdb, maximum 10, vote count. Not given when TMDB has no score (0 votes).
// TMDB does not provide IMDb's score (only another source such as OMDb does), so this plugin has only one tmdb entry; community_rating is still filled with the same score.
func tmdbRatings(average float64, votes int64) []*pluginv1.Rating {
	if !(average > 0) {
		return nil
	}
	return []*pluginv1.Rating{{Source: "tmdb", Value: average, Max: 10, Votes: max(0, votes)}}
}
