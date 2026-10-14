package main

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Look up people by name (metadata.person.search, change request 0079). Many people share a name, so confirmed is only set when confirmed by a work:
// the person's works (known_for or combined_credits) include a TMDB movie or series ID from the work hints.
//
// A single search makes at most 3 requests (not counting the one retry for a transient error inside get):
//  1. /search/person (if the name finds nothing and there are aliases, the first alias is searched next, counting as the second request);
//  2. when the work hints have no TMDB ID but have an IMDb ID, /find converts it to a TMDB ID (at most 1 request);
//  3. when the search results' known_for is not enough to confirm, fetch /person/{id}/combined_credits for the few best name matches, using up the remaining budget.

const (
	personSearchBudget = 3 // cap on requests per search
	maxPersonCandidate = 5 // cap on the number of candidates returned
)

type tmdbPersonHit struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Gender      int       `json:"gender"`
	ProfilePath string    `json:"profile_path"`
	KnownFor    []workRef `json:"known_for"`
}

type tmdbCombinedCredits struct {
	Cast []workRef `json:"cast"`
	Crew []workRef `json:"crew"`
}

type workRef struct {
	ID        int    `json:"id"`
	MediaType string `json:"media_type"` // movie or tv
}

// workKey is one TMDB work; movie and series IDs overlap, so it carries the kind.
type workKey struct {
	kind string
	id   int
}

func workKind(t pluginv1.ItemType) string {
	switch t {
	case pluginv1.ItemType_ITEM_TYPE_MOVIE:
		return "movie"
	case pluginv1.ItemType_ITEM_TYPE_SERIES:
		return "tv"
	}
	return ""
}

// SearchPerson implements pluginv1.MetadataProviderServer.
func (p *provider) SearchPerson(ctx context.Context, req *pluginv1.SearchPersonRequest) (*pluginv1.SearchPersonResponse, error) {
	c, adult, err := p.ready()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, invalidArgument("person name is required")
	}
	lang := chain(req.GetLanguages())[0].String()
	budget := personSearchBudget
	search := func(query string) ([]tmdbPersonHit, error) {
		budget--
		var r struct {
			Results []tmdbPersonHit `json:"results"`
		}
		q := url.Values{"query": {query}, "language": {lang}, "include_adult": {strconv.FormatBool(adult)}}
		if err := c.get(ctx, "/search/person", q, &r); err != nil {
			return nil, p.upstream(c, err, "person search")
		}
		return r.Results, nil
	}
	hits, err := search(name)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		if alias := firstOther(req.GetAliases(), name); alias != "" {
			if hits, err = search(alias); err != nil {
				return nil, err
			}
		}
	}
	out := &pluginv1.SearchPersonResponse{}
	if len(hits) == 0 {
		return out, nil
	}
	// Those whose name (or alias) is exactly the same come first; the rest keep TMDB's order.
	names := append([]string{name}, req.GetAliases()...)
	exact := func(h tmdbPersonHit) bool {
		return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(strings.TrimSpace(n), h.Name) })
	}
	slices.SortStableFunc(hits, func(a, b tmdbPersonHit) int {
		switch ea, eb := exact(a), exact(b); {
		case ea && !eb:
			return -1
		case !ea && eb:
			return 1
		}
		return 0
	})
	hits = hits[:min(len(hits), maxPersonCandidate)]

	works, err := p.workKeys(ctx, c, req.GetWorks(), lang, &budget)
	if err != nil {
		return nil, err
	}
	confirmed := make([]bool, len(hits))
	if len(works) > 0 {
		for i, h := range hits {
			confirmed[i] = slices.ContainsFunc(h.KnownFor, func(k workRef) bool { return works[workKey{k.MediaType, k.ID}] })
		}
		for i, h := range hits {
			if confirmed[i] || budget <= 0 {
				continue
			}
			budget--
			var cc tmdbCombinedCredits
			if err := c.get(ctx, "/person/"+strconv.Itoa(h.ID)+"/combined_credits", url.Values{"language": {lang}}, &cc); err != nil {
				if errors.Is(err, errNotFound) { // the person was just deleted or merged: treat as not confirmed
					continue
				}
				return nil, p.upstream(c, err, "person credits")
			}
			confirmed[i] = slices.ContainsFunc(slices.Concat(cc.Cast, cc.Crew), func(w workRef) bool { return works[workKey{w.MediaType, w.ID}] })
		}
	}
	for i, h := range hits {
		id := strconv.Itoa(h.ID)
		m := &pluginv1.PersonMetadata{Id: id, Name: h.Name, Gender: gender(h.Gender), ProviderIds: map[string]string{"tmdb": id}}
		if u := imageURL(h.ProfilePath); u != "" {
			m.Images = []*pluginv1.Image{{Type: pluginv1.ImageType_IMAGE_TYPE_PRIMARY, Url: u}}
		}
		out.Candidates = append(out.Candidates, &pluginv1.PersonCandidate{Person: m, Confirmed: confirmed[i]})
	}
	return out, nil
}

// workKeys converts the work hints into a set of TMDB works: those with a TMDB ID are used directly; when none has one, the first work with an IMDb ID is converted through /find (at most 1 request).
func (p *provider) workKeys(ctx context.Context, c *tmdbClient, works []*pluginv1.PersonWork, lang string, budget *int) (map[workKey]bool, error) {
	keys := map[workKey]bool{}
	for _, w := range works {
		kind := workKind(w.GetItemType())
		if id, err := strconv.Atoi(w.GetProviderIds()["tmdb"]); err == nil && kind != "" && id > 0 {
			keys[workKey{kind, id}] = true
		}
	}
	if len(keys) > 0 || *budget <= 0 {
		return keys, nil
	}
	for _, w := range works {
		imdb := w.GetProviderIds()["imdb"]
		if imdb == "" || workKind(w.GetItemType()) == "" {
			continue
		}
		*budget--
		var found struct {
			Movies []struct {
				ID int `json:"id"`
			} `json:"movie_results"`
			TV []struct {
				ID int `json:"id"`
			} `json:"tv_results"`
		}
		if err := c.get(ctx, "/find/"+url.PathEscape(imdb), url.Values{"external_source": {"imdb_id"}, "language": {lang}}, &found); err != nil {
			if errors.Is(err, errNotFound) {
				return keys, nil
			}
			return nil, p.upstream(c, err, "find")
		}
		for _, m := range found.Movies {
			keys[workKey{"movie", m.ID}] = true
		}
		for _, t := range found.TV {
			keys[workKey{"tv", t.ID}] = true
		}
		break
	}
	return keys, nil
}

// firstOther returns the first non-empty alias that differs from name (case-insensitive).
func firstOther(aliases []string, name string) string {
	for _, a := range aliases {
		if a = strings.TrimSpace(a); a != "" && !strings.EqualFold(a, name) {
			return a
		}
	}
	return ""
}
