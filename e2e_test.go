package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// TestRealTMDB runs end to end against the real TMDB, querying only well-known public items (no library content).
// It needs the environment variable TMDB_E2E_KEY_FILE pointing to a file holding the key or token (outside the repo, 0600); skipped when unset.
// The log records only a summary of fields, never the key.
func TestRealTMDB(t *testing.T) {
	path := os.Getenv("TMDB_E2E_KEY_FILE")
	if path == "" {
		t.Skip("TMDB_E2E_KEY_FILE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key file: %v", err) // the error contains only the path
	}
	key := strings.TrimSpace(string(raw))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := newProvider()
	resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: key}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure: %v %v", resp.GetFieldErrors(), err)
	}
	zhTW := &pluginv1.Languages{Language: "zh-TW", Fallbacks: []string{"zh-CN"}}

	search, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Fight Club", Year: 1999, Languages: zhTW})
	if err != nil || len(search.GetCandidates()) == 0 || search.GetCandidates()[0].GetId() != "550" {
		t.Errorf("search Fight Club: %v %v", search.GetCandidates(), err)
	}
	byIMDb, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"imdb": "tt0137523"}, Languages: zhTW})
	if err != nil || len(byIMDb.GetCandidates()) != 1 || byIMDb.GetCandidates()[0].GetId() != "550" {
		t.Errorf("imdb hint: %v %v", byIMDb.GetCandidates(), err)
	}

	movie, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550", Languages: zhTW})
	if err != nil {
		t.Fatalf("movie 550: %v", err)
	}
	t.Logf("movie: %q year=%d runtime=%ds rating=%q imdb=%s genres=%v credits=%d images=%d first-poster-lang=%q",
		movie.GetName(), movie.GetProductionYear(), movie.GetRuntimeSeconds(), movie.GetOfficialRating(),
		movie.GetProviderIds()["imdb"], movie.GetGenres(), len(movie.GetCredits()), len(movie.GetImages()), firstLang(movie.GetImages(), pluginv1.ImageType_IMAGE_TYPE_PRIMARY))
	if movie.GetProviderIds()["imdb"] != "tt0137523" || movie.GetOverview() == "" || len(movie.GetCredits()) == 0 {
		t.Errorf("movie 550 incomplete: %v", movie)
	}

	series, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Id: "1399", Languages: zhTW})
	if err != nil {
		t.Fatalf("series 1399: %v", err)
	}
	t.Logf("series: %q premiere=%s end=%s tvdb=%s credits=%d images=%d", series.GetName(), series.GetPremiereDate(),
		series.GetEndDate(), series.GetProviderIds()["tvdb"], len(series.GetCredits()), len(series.GetImages()))
	if series.GetEndDate() == "" || len(series.GetCredits()) < 20 {
		t.Errorf("series 1399: ended series should have EndDate and aggregate credits: end=%q credits=%d",
			series.GetEndDate(), len(series.GetCredits()))
	}

	s1 := int32(1)
	season, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_SEASON, SeriesId: "1399", SeasonNumber: &s1, Languages: zhTW})
	if err != nil {
		t.Fatalf("season: %v", err)
	}
	t.Logf("season: %q index=%d premiere=%s images=%d", season.GetName(), season.GetIndexNumber(), season.GetPremiereDate(), len(season.GetImages()))

	episode, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399", SeasonNumber: &s1, EpisodeNumber: &s1, Languages: zhTW})
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	t.Logf("episode: %q S%02dE%02d aired=%s runtime=%ds credits=%d images=%d", episode.GetName(), episode.GetParentIndexNumber(),
		episode.GetIndexNumber(), episode.GetPremiereDate(), episode.GetRuntimeSeconds(), len(episode.GetCredits()), len(episode.GetImages()))
	if episode.GetIndexNumber() != 1 || episode.GetParentIndexNumber() != 1 {
		t.Errorf("episode numbers: %v", episode)
	}

	person, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287", Languages: zhTW})
	if err != nil {
		t.Fatalf("person 287: %v", err)
	}
	t.Logf("person: %q born=%s gender=%s imdb=%s links=%d images=%d", person.GetName(), person.GetBirthDate(), person.GetGender(),
		person.GetProviderIds()["imdb"], len(person.GetLinks()), len(person.GetImages()))
	if person.GetProviderIds()["imdb"] != "nm0000093" || person.GetBirthDate() != "1963-12-18" {
		t.Errorf("person 287: %v", person)
	}

	// Person backdrops (host feature person_backdrop): the wide pictures person 287 is tagged in. The log shows what kinds of pictures TMDB tags
	// (counts per image_type) and what the plugin keeps; nothing is printed that could identify a library.
	t.Setenv("KRYSPAL_HOST_FEATURES", "person_backdrop")
	var tagged tmdbTagged
	if c, _, err := p.ready(); err == nil {
		if err := c.get(ctx, "/person/287/tagged_images", nil, &tagged); err != nil {
			t.Errorf("tagged_images: %v", err)
		}
	}
	kinds := map[string]int{}
	for _, r := range tagged.Results {
		kinds[r.ImageType]++
	}
	backdrops, err := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "287"},
		Types: []pluginv1.ImageType{pluginv1.ImageType_IMAGE_TYPE_BACKDROP}})
	if err != nil {
		t.Fatalf("person backdrops: %v", err)
	}
	t.Logf("tagged pictures: %d on this page, by image_type %v; kept %d backdrops", len(tagged.Results), kinds, len(backdrops.GetImages()))
	if len(backdrops.GetImages()) > 0 {
		b := backdrops.GetImages()[0]
		t.Logf("first backdrop: %dx%d type=%v", b.GetWidth(), b.GetHeight(), b.GetType())
	}

	col, err := p.GetCollection(ctx, &pluginv1.GetCollectionRequest{Id: "10", Languages: zhTW})
	if err != nil {
		t.Fatalf("collection 10: %v", err)
	}
	t.Logf("collection: %q parts=%d first=%q images=%d", col.GetName(), len(col.GetParts()), firstName(col.GetParts()), len(col.GetImages()))
	if len(col.GetParts()) < 6 {
		t.Errorf("collection 10 has %d parts", len(col.GetParts()))
	}

	// A measured case from the research report: when the Traditional Chinese title is missing TMDB returns the original name (ゴジラ), and the plugin must use the Taiwan alias or the converted Simplified Chinese title.
	for _, lang := range []string{"zh-TW", "zh-CN", "zh-HK", "ja", "en-US"} {
		g, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "1678",
			Languages: &pluginv1.Languages{Language: lang}})
		if err != nil {
			t.Fatalf("movie 1678 %s: %v", lang, err)
		}
		t.Logf("1678 %s: %q genres=%v first-poster-lang=%q", lang, g.GetName(), g.GetGenres(), firstLang(g.GetImages(), pluginv1.ImageType_IMAGE_TYPE_PRIMARY))
		if strings.HasPrefix(lang, "zh") && (!hasHan(g.GetName()) || g.GetName() == "ゴジラ") {
			t.Errorf("1678 %s: name %q is not a Chinese title", lang, g.GetName())
		}
	}
	// House (1408): Traditional Chinese episode names are mostly the placeholder "Episode N"; the plugin should return the real episode name (fallback chain) or an empty string, never a placeholder.
	for _, lang := range []string{"zh-TW", "zh-CN"} {
		for _, n := range []int32{1, 2, 3} {
			e := n
			ep, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1408",
				SeasonNumber: &s1, EpisodeNumber: &e, Languages: &pluginv1.Languages{Language: lang}})
			if err != nil {
				t.Fatalf("1408 S01E%02d %s: %v", n, lang, err)
			}
			t.Logf("1408 S01E%02d %s: %q", n, lang, ep.GetName())
			if isPlaceholder(ep.GetName()) {
				t.Errorf("1408 S01E%02d %s: placeholder name %q", n, lang, ep.GetName())
			}
		}
	}
	// Trailers, airing status, actual language.
	t.Logf("movie 550 trailers=%d first=%v language=%q; series 1399 status=%s language=%q", len(movie.GetTrailers()),
		movie.GetTrailers()[:min(1, len(movie.GetTrailers()))], movie.GetLanguage(), series.GetStatus(), series.GetLanguage())
	if len(movie.GetTrailers()) == 0 || !strings.HasPrefix(movie.GetTrailers()[0].GetUrl(), "https://www.youtube.com/watch?v=") {
		t.Errorf("movie 550 has no YouTube trailer: %v", movie.GetTrailers())
	}
	if series.GetStatus() != pluginv1.SeriesStatus_SERIES_STATUS_ENDED || movie.GetLanguage() != "zh-TW" {
		t.Errorf("status %v, movie language %q", series.GetStatus(), movie.GetLanguage())
	}
	// Simplified Chinese target: person names and genres are Simplified.
	cnPerson, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287", Languages: &pluginv1.Languages{Language: "zh-CN"}})
	if err != nil {
		t.Fatalf("person 287 zh-CN: %v", err)
	}
	t.Logf("person zh-CN: %q", cnPerson.GetName())
	cnMovie, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550", Languages: &pluginv1.Languages{Language: "zh-CN"}})
	if err != nil {
		t.Fatalf("movie 550 zh-CN: %v", err)
	}
	t.Logf("movie zh-CN: %q genres=%v rating=%q", cnMovie.GetName(), cnMovie.GetGenres(), cnMovie.GetOfficialRating())
	if !slices.Contains(cnMovie.GetGenres(), "剧情") || !slices.Contains(movie.GetGenres(), "劇情") {
		t.Errorf("genres: zh-CN %v zh-TW %v", cnMovie.GetGenres(), movie.GetGenres())
	}
	// Name search (Traditional Chinese query).
	byName, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Name: "權力遊戲", Languages: zhTW})
	if err != nil {
		t.Fatalf("search 權力遊戲: %v", err)
	}
	t.Logf("search 權力遊戲: first=%q score=%.2f", firstName(byName.GetCandidates()), firstScore(byName.GetCandidates()))
}

func firstLang(imgs []*pluginv1.Image, kind pluginv1.ImageType) string {
	for _, i := range imgs {
		if i.GetType() == kind {
			return i.GetLanguage()
		}
	}
	return ""
}

func firstScore(c []*pluginv1.SearchCandidate) float64 {
	if len(c) == 0 {
		return 0
	}
	return c[0].GetScore()
}

func firstName(c []*pluginv1.SearchCandidate) string {
	if len(c) == 0 {
		return ""
	}
	return c[0].GetName()
}
