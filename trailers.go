package main

import (
	"cmp"
	"net/url"
	"slices"
	"strings"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Trailers (change request 0022): only YouTube Trailer and Teaser (the forms an Emby RemoteTrailers client can open).

// maxTrailers is the cap on the number of trailers returned.
const maxTrailers = 5

type tmdbVideo struct {
	Key         string `json:"key"`
	Site        string `json:"site"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Official    bool   `json:"official"`
	Lang        string `json:"iso_639_1"`
	Region      string `json:"iso_3166_1"`
	PublishedAt string `json:"published_at"`
}

type tmdbVideos struct {
	Results []tmdbVideo `json:"results"`
}

// videoLanguages is include_video_language: the fallback chain's languages (two letters) plus null (videos with no language label).
func videoLanguages(ch []locale) string {
	var codes []string
	for _, l := range ch {
		if !slices.Contains(codes, l.lang) {
			codes = append(codes, l.lang)
		}
	}
	return strings.Join(append(codes, "null"), ",")
}

// trailers converts trailers. Order: language (target language with target region → target language → fallback chain order → no language label → others),
// Trailer before Teaser, official first, newer publication first; at most maxTrailers.
func trailers(v *tmdbVideos, ch []locale) []*pluginv1.Trailer {
	if v == nil {
		return nil
	}
	rank := func(x tmdbVideo) int {
		for i, l := range ch {
			if x.Lang == l.lang {
				if i == 0 && l.region != "" && x.Region == l.region {
					return 0
				}
				return 1 + i
			}
		}
		if x.Lang == "" {
			return len(ch) + 1
		}
		return len(ch) + 2
	}
	var list []tmdbVideo
	for _, x := range v.Results {
		if x.Site == "YouTube" && x.Key != "" && (x.Type == "Trailer" || x.Type == "Teaser") {
			list = append(list, x)
		}
	}
	published := func(x tmdbVideo) time.Time { t, _ := time.Parse(time.RFC3339, x.PublishedAt); return t }
	slices.SortStableFunc(list, func(a, b tmdbVideo) int {
		if c := cmp.Compare(rank(a), rank(b)); c != 0 {
			return c
		}
		if c := cmp.Compare(boolRank(a.Type == "Trailer"), boolRank(b.Type == "Trailer")); c != 0 {
			return c
		}
		if c := cmp.Compare(boolRank(a.Official), boolRank(b.Official)); c != 0 {
			return c
		}
		return published(b).Compare(published(a))
	})
	var out []*pluginv1.Trailer
	seen := map[string]bool{}
	for _, x := range list {
		if seen[x.Key] || len(out) >= maxTrailers {
			continue
		}
		seen[x.Key] = true
		lang := x.Lang
		if lang != "" && x.Region != "" {
			lang += "-" + x.Region
		}
		out = append(out, &pluginv1.Trailer{Url: "https://www.youtube.com/watch?v=" + url.QueryEscape(x.Key), Name: x.Name,
			Language: lang, Kind: strings.ToLower(x.Type), Official: x.Official})
	}
	return out
}

// boolRank puts true first.
func boolRank(b bool) int {
	if b {
		return 0
	}
	return 1
}
