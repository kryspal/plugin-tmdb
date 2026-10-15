package main

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

// People and collections. A person's name does not change with the language parameter (TMDB stores only one main name); translated names are in translations and also_known_as.

type tmdbPerson struct {
	ID           int               `json:"id"`
	Name         string            `json:"name"`
	AlsoKnownAs  []string          `json:"also_known_as"`
	Biography    string            `json:"biography"`
	Birthday     string            `json:"birthday"`
	Deathday     string            `json:"deathday"`
	PlaceOfBirth string            `json:"place_of_birth"`
	Gender       int               `json:"gender"` // 0 unknown, 1 female, 2 male, 3 non-binary
	Homepage     string            `json:"homepage"`
	IMDbID       string            `json:"imdb_id"`
	ProfilePath  string            `json:"profile_path"`
	ExternalIDs  *tmdbExternalIDs  `json:"external_ids"`
	Images       *tmdbImages       `json:"images"`
	Translations *tmdbTranslations `json:"translations"`
}

// GetPerson implements pluginv1.MetadataProviderServer. One request brings back external IDs, photos and translations.
func (p *provider) GetPerson(ctx context.Context, req *pluginv1.GetPersonRequest) (*pluginv1.PersonMetadata, error) {
	sz := p.imageSizes()
	c, _, err := p.ready()
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, invalidArgument("person id is required")
	}
	ch := chain(req.GetLanguages())
	target := ch[0]
	var pe tmdbPerson
	q := url.Values{"language": {target.String()}, "append_to_response": {"external_ids,images,translations"}}
	if err := c.get(ctx, "/person/"+url.PathEscape(req.GetId()), q, &pe); err != nil {
		return nil, p.upstream(c, err, "person")
	}
	var ts []tmdbTranslation
	if pe.Translations != nil {
		ts = pe.Translations.Translations
	}
	id := strconv.Itoa(pe.ID)
	m := &pluginv1.PersonMetadata{Id: id, Name: p.personName(pe, ts, ch), Aliases: pe.AlsoKnownAs,
		BirthDate: pe.Birthday, DeathDate: pe.Deathday, BirthPlace: pe.PlaceOfBirth,
		Gender:      gender(pe.Gender),
		ProviderIds: map[string]string{"tmdb": id},
		Links:       []*pluginv1.Link{{Kind: "tmdb", Url: "https://www.themoviedb.org/person/" + id}},
	}
	if v, ok := pick(ts, ch, func(t tmdbTranslation) string { return t.Data.Biography }, nonEmpty); ok {
		m.Overview = p.localize(v, target)
	} else {
		m.Overview = pe.Biography
	}
	imdb := pe.IMDbID
	if ids := pe.ExternalIDs; ids != nil {
		imdb = first(imdb, ids.IMDb)
		for kind, handle := range map[string]string{"twitter": ids.Twitter, "instagram": ids.Instagram, "facebook": ids.Facebook} {
			if handle != "" {
				m.Links = append(m.Links, &pluginv1.Link{Kind: kind, Url: "https://www." + kind + ".com/" + url.PathEscape(handle)})
			}
		}
		if ids.Wikidata != "" {
			m.ProviderIds["wikidata"] = ids.Wikidata
		}
	}
	if imdb != "" {
		m.ProviderIds["imdb"] = imdb
		m.Links = append(m.Links, &pluginv1.Link{Kind: "imdb", Url: "https://www.imdb.com/name/" + url.PathEscape(imdb) + "/"})
	}
	if pe.Homepage != "" {
		m.Links = append(m.Links, &pluginv1.Link{Kind: "homepage", Url: pe.Homepage})
	}
	slices.SortStableFunc(m.Links, func(a, b *pluginv1.Link) int { return strings.Compare(a.Kind, b.Kind) })
	var profiles []tmdbImage
	if pe.Images != nil {
		profiles = pe.Images.Profiles
	}
	m.Images = convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, profiles, pe.ProfilePath, ch, sz.profile)
	return m, nil
}

// personName picks the person's name: the target language's translation → a translation in the same language from another region on the fallback chain (converted to the target script for Chinese) →
// (Chinese) a Chinese alias in also_known_as (see chineseAlias) → the main name.
// Translations in other languages are not used: with an English target, replacing a Japanese actor's name with another foreign-language form is meaningless, and the main name is usually the romanization.
func (p *provider) personName(pe tmdbPerson, ts []tmdbTranslation, ch []locale) string {
	target := ch[0]
	if v, ok := pick(ts, sameLanguage(ch), func(t tmdbTranslation) string { return t.Data.Name }, nonEmpty); ok {
		return p.localize(v, target)
	}
	if target.lang == "zh" {
		if a := p.chineseAlias(pe.AlsoKnownAs, target); a != "" {
			return a
		}
	}
	return pe.Name
}

// chineseAlias picks a Chinese name from also_known_as. Aliases carry no region, so the script is judged by conversion: one that changes when converted to the other script
// is certainly in the target script and has top priority; one written the same in both scripts comes next; one certainly in the other script is converted to the target script and used.
func (p *provider) chineseAlias(akas []string, target locale) string {
	other := locale{lang: "zh", region: "CN"}
	if target.script() == "Hans" {
		other = locale{lang: "zh", region: "TW"}
	}
	var neutral, foreign string
	for _, a := range akas {
		a = strings.TrimSpace(a)
		if !hasHan(a) || !onlyHanAndPunct(a) {
			continue
		}
		switch {
		case p.fold.convert(a, target, other) != a:
			return a
		case p.fold.convert(a, other, target) == a:
			neutral = first(neutral, a)
		default:
			foreign = first(foreign, a)
		}
	}
	return first(neutral, p.conv.convert(foreign, other, target))
}

// onlyHanAndPunct excludes aliases that mix in kana or hangul (the Japanese and Korean forms are not Chinese translated names).
func onlyHanAndPunct(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x3040 && r <= 0x30ff, r >= 0xac00 && r <= 0xd7af, r >= 0x1100 && r <= 0x11ff:
			return false
		}
	}
	return true
}

type tmdbCollection struct {
	ID           int                `json:"id"`
	Name         string             `json:"name"`
	Overview     string             `json:"overview"`
	PosterPath   string             `json:"poster_path"`
	BackdropPath string             `json:"backdrop_path"`
	Parts        []tmdbSearchResult `json:"parts"`
	Images       *tmdbImages        `json:"images"`
	Translations *tmdbTranslations  `json:"translations"`
}

// gender converts TMDB's gender number (0 unknown, 1 female, 2 male, 3 non-binary) to the protocol enum; any other value is treated as unknown.
func gender(n int) pluginv1.Gender {
	return map[int]pluginv1.Gender{1: pluginv1.Gender_GENDER_FEMALE, 2: pluginv1.Gender_GENDER_MALE, 3: pluginv1.Gender_GENDER_OTHER}[n]
}

// GetCollection implements pluginv1.MetadataProviderServer. Members are in ascending order of release date.
func (p *provider) GetCollection(ctx context.Context, req *pluginv1.GetCollectionRequest) (*pluginv1.CollectionMetadata, error) {
	sz := p.imageSizes()
	c, _, err := p.ready()
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, invalidArgument("collection id is required")
	}
	ch := chain(req.GetLanguages())
	target := ch[0]
	var col tmdbCollection
	q := url.Values{"language": {target.String()}, "append_to_response": {"images,translations"}, "include_image_language": {imageLanguages(ch)}}
	if err := c.get(ctx, "/collection/"+url.PathEscape(req.GetId()), q, &col); err != nil {
		return nil, p.upstream(c, err, "collection")
	}
	var ts []tmdbTranslation
	if col.Translations != nil {
		ts = col.Translations.Translations
	}
	id := strconv.Itoa(col.ID)
	m := &pluginv1.CollectionMetadata{Id: id, Name: col.Name, Overview: col.Overview, ProviderIds: map[string]string{"tmdb": id}}
	if v, ok := pick(ts, ch, tmdbTranslation.title, nonEmpty); ok {
		m.Name = p.localize(v, target)
	}
	if v, ok := pick(ts, ch, func(t tmdbTranslation) string { return t.Data.Overview }, nonEmpty); ok {
		m.Overview = p.localize(v, target)
	}
	m.Name = p.collectionName(m.Name)
	var imgs tmdbImages
	if col.Images != nil {
		imgs = *col.Images
	}
	m.Images = append(convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, imgs.Posters, col.PosterPath, ch, sz.poster),
		capImages(convertImages(pluginv1.ImageType_IMAGE_TYPE_BACKDROP, imgs.Backdrops, col.BackdropPath, ch, sz.backdrop), p.backdropLimit())...)
	parts := slices.Clone(col.Parts)
	slices.SortStableFunc(parts, func(a, b tmdbSearchResult) int {
		// those without a date go last.
		switch {
		case a.ReleaseDate == "" && b.ReleaseDate != "":
			return 1
		case a.ReleaseDate != "" && b.ReleaseDate == "":
			return -1
		}
		return strings.Compare(a.ReleaseDate, b.ReleaseDate)
	})
	for _, part := range parts {
		m.Parts = append(m.Parts, candidate(part, pluginv1.ItemType_ITEM_TYPE_MOVIE, sz.poster))
	}
	return m, nil
}

// ---- helpers ----

func year(date string) int32 {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return int32(y)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func invalidArgument(msg string) error { return sdk.InvalidArgument(msg) }
