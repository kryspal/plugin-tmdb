package main

import (
	"strconv"
	"strings"

	"github.com/kryspal/plugin-sdk/i18n"
	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Image download sizes (settings poster_size, backdrop_size and profile_size). The values are TMDB's own size names from its /configuration endpoint.
// Only sizes that still look sharp for the server's uses are offered: thumbnails up to 720 px wide, a 1600 px lightbox, full-width backdrops and
// profile photos up to about 480 px wide. Smaller TMDB sizes (for example w300 backdrops or w185 profiles) are deliberately not offered.
const (
	sizeOriginal = "original"

	keyPosterSize   = "poster_size"
	keyBackdropSize = "backdrop_size"
	keyProfileSize  = "profile_size"
)

var (
	posterSizes   = []string{sizeOriginal, "w780"}
	backdropSizes = []string{sizeOriginal, "w1280"}
	profileSizes  = []string{sizeOriginal, "h632"}
)

// imageSizes is the configured size for each kind of image. The zero value means original everywhere (the behavior before the settings existed).
type imageSizes struct {
	poster, backdrop, profile string
}

// validSize reports whether v is empty (the default) or one of the offered sizes.
func validSize(v string, offered []string) bool {
	if v == "" {
		return true
	}
	for _, s := range offered {
		if s == v {
			return true
		}
	}
	return false
}

// sizedURL builds the CDN URL of path at the given TMDB size; an empty size means original.
func sizedURL(path, size string) string {
	if path == "" {
		return ""
	}
	if size == "" {
		size = sizeOriginal
	}
	return imageBase + size + path
}

// scaleDims returns the dimensions of an image of w x h after TMDB resizes it to size ("w1280" fixes the width, "h632" the height).
// Original, or unknown source dimensions, return them unchanged. TMDB never upscales, so a smaller source keeps its size.
func scaleDims(w, h int, size string) (int, int) {
	if size == "" || size == sizeOriginal || w <= 0 || h <= 0 || len(size) < 2 {
		return w, h
	}
	n, err := strconv.Atoi(size[1:])
	if err != nil || n <= 0 {
		return w, h
	}
	switch {
	case strings.HasPrefix(size, "w") && w > n:
		return n, (h*n + w/2) / w
	case strings.HasPrefix(size, "h") && h > n:
		return (w*n + h/2) / h, n
	}
	return w, h
}

// sizeField builds the select field of one image kind. Original is the default; the help text (shared by the three fields) says what a smaller size costs.
func sizeField(key, msg string, offered []string, t i18n.Translator) *pluginv1.ConfigField {
	f := &pluginv1.ConfigField{
		Key: key, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_SELECT, Group: "advanced", DefaultValue: sizeOriginal,
		Label: t.T(msg + "Label"), Description: t.T("imageSizeHelp"),
	}
	for _, s := range offered {
		f.Options = append(f.Options, &pluginv1.ConfigOption{Value: s, Label: t.T(msg + "." + s)})
	}
	return f
}
