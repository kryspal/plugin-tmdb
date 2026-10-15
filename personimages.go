package main

import (
	"context"
	"errors"
	"net/url"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// GetImages implements pluginv1.ImageProviderServer for the capability images.person and nothing else: TMDB's movie and series
// images come with GetItem. It returns all the profile photos TMDB has for the person, best first (the same order as GetPerson uses:
// rating, then resolution). The person is found by the "tmdb" provider ID only; a name is not searched here (a wrong match would put
// someone else's face on a person), the host has SearchPerson for finding the ID.
func (p *provider) GetImages(ctx context.Context, req *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	if req.GetType() != pluginv1.ItemType_ITEM_TYPE_PERSON {
		return nil, invalidArgument("only person photos are served by this plugin (images.person)")
	}
	c, _, err := p.ready()
	if err != nil {
		return nil, err
	}
	id := req.GetProviderIds()["tmdb"]
	if id == "" {
		return &pluginv1.GetImagesResponse{}, nil
	}
	var imgs tmdbImages
	if err := c.get(ctx, "/person/"+url.PathEscape(id)+"/images", nil, &imgs); err != nil {
		if errors.Is(err, errNotFound) {
			return &pluginv1.GetImagesResponse{}, nil
		}
		return nil, p.upstream(c, err, "person images")
	}
	out := convertImages(pluginv1.ImageType_IMAGE_TYPE_PRIMARY, imgs.Profiles, "", chain(req.GetLanguages()), p.imageSizes().profile)
	if n := int(req.GetLimitPerType()); n > 0 && len(out) > n {
		out = out[:n]
	}
	return &pluginv1.GetImagesResponse{Images: out}, nil
}
