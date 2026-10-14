package main

import (
	"cmp"
	"slices"
	"strings"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Image order (the protocol convention is that the first image of each kind is the preferred one). TMDB images carry a language (iso_639_1) and a region (iso_3166_1, live since about 2025-10);
// the region is often mislabeled, so it is only used for ordering, never for filtering. Both xx and null mean no text.

type tmdbImage struct {
	FilePath string  `json:"file_path"`
	Lang     string  `json:"iso_639_1"`
	Region   string  `json:"iso_3166_1"`
	Vote     float64 `json:"vote_average"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
}

type tmdbImages struct {
	Posters   []tmdbImage `json:"posters"`
	Backdrops []tmdbImage `json:"backdrops"`
	Logos     []tmdbImage `json:"logos"`
	Stills    []tmdbImage `json:"stills"`
	Profiles  []tmdbImage `json:"profiles"`
}

func (i tmdbImage) textless() bool { return i.Lang == "" || i.Lang == "xx" }

// imageLanguages is include_image_language: the languages on the fallback chain (two letters) plus xx and null for images without text.
func imageLanguages(ch []locale) string {
	var codes []string
	for _, l := range ch {
		if !slices.Contains(codes, l.lang) {
			codes = append(codes, l.lang)
		}
	}
	return strings.Join(append(codes, "xx", "null"), ",")
}

// imageRank returns an image's rank (lower is preferred).
//
//   - Images with text (posters, logos, textual is true):
//     target language with target region → target language with a same-script region (Chinese) → target language without region → other regions of the target language →
//     other languages on the fallback chain → no text → others.
//     For example zh-TW: zh-TW → zh-HK, zh-MO → zh → zh-CN, zh-SG → en → no text → others.
//   - Backdrops: those without text come first, the rest as above.
func imageRank(i tmdbImage, ch []locale, textual bool) int {
	const textless, other = 50, 100
	if i.textless() {
		if textual {
			return textless
		}
		return -1
	}
	target := ch[0]
	if i.Lang == target.lang {
		img := locale{lang: i.Lang, region: i.Region}
		switch {
		case i.Region == target.region:
			return 0
		case target.lang == "zh" && i.Region != "" && img.script() == target.script():
			return 1
		case i.Region == "":
			return 2
		}
		return 3
	}
	for n, l := range ch[1:] {
		if i.Lang == l.lang {
			return 4 + n
		}
	}
	return other
}

// sortImages sorts by imageRank; within the same rank it sorts by rating, then by descending resolution. SVGs are skipped (most clients do not support them).
func sortImages(imgs []tmdbImage, ch []locale, textual bool) []tmdbImage {
	out := slices.DeleteFunc(slices.Clone(imgs), func(i tmdbImage) bool {
		return strings.HasSuffix(strings.ToLower(i.FilePath), ".svg")
	})
	slices.SortStableFunc(out, func(a, b tmdbImage) int {
		if c := cmp.Compare(imageRank(a, ch, textual), imageRank(b, ch, textual)); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Vote, a.Vote); c != 0 {
			return c
		}
		return cmp.Compare(b.Width*b.Height, a.Width*a.Height)
	})
	return out
}

// imageLanguage is the protocol's Image.language: a BCP 47 language-region when there is a region, an empty string when the image has no text.
func imageLanguage(i tmdbImage) string {
	switch {
	case i.textless():
		return ""
	case i.Region != "":
		return i.Lang + "-" + i.Region
	}
	return i.Lang
}

// convertImages sorts by kind and converts to the protocol format; fallback is the detail's own poster_path and so on, used when images has no data.
func convertImages(kind pluginv1.ImageType, imgs []tmdbImage, fallback string, ch []locale) []*pluginv1.Image {
	textual := kind == pluginv1.ImageType_IMAGE_TYPE_PRIMARY || kind == pluginv1.ImageType_IMAGE_TYPE_LOGO
	var out []*pluginv1.Image
	for _, i := range sortImages(imgs, ch, textual) {
		out = append(out, &pluginv1.Image{Type: kind, Url: imageURL(i.FilePath), Language: imageLanguage(i),
			Width: int32(i.Width), Height: int32(i.Height)})
	}
	if len(out) == 0 && fallback != "" {
		out = append(out, &pluginv1.Image{Type: kind, Url: imageURL(fallback)})
	}
	return out
}
