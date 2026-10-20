package main

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"

	pluginapi "github.com/kryspal/plugin-sdk"
	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// hostFeaturePersonBackdrop is the host feature name. It is written out because the SDK this plugin is built with (v0.2.0) predates the
// constant; the plugin needs no newer SDK, only a host that lists the feature.
const hostFeaturePersonBackdrop = "person_backdrop"

const (
	// Person backdrops come from the pictures TMDB users tagged the person in. Only wide, large ones qualify: a person backdrop is
	// shown full width, and a tagged poster or a small still would look wrong.
	backdropMinAspect = 1.5
	backdropMinWidth  = 1280
	// defaultPersonBackdrops is how many backdrop candidates are returned when the host sets no limit; one page of tagged images is at most 20.
	defaultPersonBackdrops = 10
)

// tmdbTagged is the answer of /person/{id}/tagged_images: pictures of movies and episodes the person is tagged in.
type tmdbTagged struct {
	Results []struct {
		tmdbImage
		ImageType   string  `json:"image_type"`
		AspectRatio float64 `json:"aspect_ratio"`
		VoteCount   int     `json:"vote_count"`
		Media       struct {
			Adult bool `json:"adult"`
		} `json:"media"`
	} `json:"results"`
}

// personBackdropsOn reports whether the host keeps a person's backdrop. Older hosts take every person image as a photo candidate, so nothing
// else is sent to them.
func personBackdropsOn() bool {
	return pluginapi.HostFeatures(os.Getenv)[hostFeaturePersonBackdrop]
}

// wants reports whether the request asks for the type (an empty list asks for all).
func wants(types []pluginv1.ImageType, any ...pluginv1.ImageType) bool {
	return len(types) == 0 || slices.ContainsFunc(types, func(t pluginv1.ImageType) bool { return slices.Contains(any, t) })
}

// GetImages implements pluginv1.ImageProviderServer for the capability images.person and nothing else: TMDB's movie and series
// images come with GetItem. The person is found by the "tmdb" provider ID only; a name is not searched here (a wrong match would put
// someone else's face on a person), the host has SearchPerson for finding the ID.
//
//   - Photos (types empty, PRIMARY or PHOTO): all the profile photos TMDB has, best first (the same order as GetPerson uses: rating, then resolution).
//   - Backdrops (types empty or BACKDROP, only on hosts with person_backdrop): wide pictures the person is tagged in, best first. They are offered
//     only here, so an administrator picks one by hand; GetPerson does not carry them, because tagged pictures are group shots and scenes whose
//     quality cannot be judged automatically.
func (p *provider) GetImages(ctx context.Context, req *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	if req.GetType() != pluginv1.ItemType_ITEM_TYPE_PERSON {
		return nil, invalidArgument("only person photos are served by this plugin (images.person)")
	}
	c, adult, err := p.ready()
	if err != nil {
		return nil, err
	}
	id := req.GetProviderIds()["tmdb"]
	if id == "" {
		return &pluginv1.GetImagesResponse{}, nil
	}
	resp := &pluginv1.GetImagesResponse{}
	if wants(req.GetTypes(), pluginv1.ImageType_IMAGE_TYPE_PRIMARY, pluginv1.ImageType_IMAGE_TYPE_PHOTO) {
		var imgs tmdbImages
		if err := c.get(ctx, "/person/"+url.PathEscape(id)+"/images", nil, &imgs); err != nil {
			if !errors.Is(err, errNotFound) {
				return nil, p.upstream(c, err, "person images")
			}
			if hostShowsNotices() {
				resp.Notices = []*pluginv1.Notice{notFoundNotice("TMDB has no photos for this person.")}
			}
		}
		out := convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, imgs.Profiles, "", chain(req.GetLanguages()), p.imageSizes().profile)
		if n := int(req.GetLimitPerType()); n > 0 && len(out) > n {
			out = out[:n]
		}
		resp.Images = out
	}
	if personBackdropsOn() && wants(req.GetTypes(), pluginv1.ImageType_IMAGE_TYPE_BACKDROP) {
		var tagged tmdbTagged
		if err := c.get(ctx, "/person/"+url.PathEscape(id)+"/tagged_images", nil, &tagged); err != nil && !errors.Is(err, errNotFound) {
			return nil, p.upstream(c, err, "person backdrops")
		}
		limit := defaultPersonBackdrops
		if n := int(req.GetLimitPerType()); n > 0 {
			limit = n
		}
		resp.Images = append(resp.Images, personBackdrops(tagged, adult, p.imageSizes().backdrop, limit)...)
	}
	return resp, nil
}

// personBackdrops picks the wide, large pictures from the tagged ones, best first: by rating, then votes, then width. Pictures of adult titles
// are dropped unless the plugin is set to include them, and a path is used once.
func personBackdrops(t tmdbTagged, includeAdult bool, size string, limit int) []*pluginv1.Image {
	type cand struct {
		img   tmdbImage
		votes int
	}
	var cands []cand
	seen := map[string]bool{}
	for _, r := range t.Results {
		if r.FilePath == "" || seen[r.FilePath] || strings.HasSuffix(strings.ToLower(r.FilePath), ".svg") {
			continue
		}
		if r.ImageType != "backdrop" && r.ImageType != "still" {
			continue
		}
		aspect := r.AspectRatio
		if aspect == 0 && r.Height > 0 {
			aspect = float64(r.Width) / float64(r.Height)
		}
		if aspect < backdropMinAspect || r.Width < backdropMinWidth || (r.Media.Adult && !includeAdult) {
			continue
		}
		seen[r.FilePath] = true
		cands = append(cands, cand{r.tmdbImage, r.VoteCount})
	}
	slices.SortStableFunc(cands, func(a, b cand) int {
		if c := cmp.Compare(b.img.Vote, a.img.Vote); c != 0 {
			return c
		}
		if c := cmp.Compare(b.votes, a.votes); c != 0 {
			return c
		}
		return cmp.Compare(b.img.Width, a.img.Width)
	})
	var out []*pluginv1.Image
	for _, c := range cands {
		if len(out) == limit {
			break
		}
		w, h := scaleDims(c.img.Width, c.img.Height, size)
		out = append(out, &pluginv1.Image{Type: pluginv1.ImageType_IMAGE_TYPE_BACKDROP, Url: sizedURL(c.img.FilePath, size), Width: int32(w), Height: int32(h)})
	}
	return out
}
