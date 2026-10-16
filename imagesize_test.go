package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// configuredWith is configured() plus extra setting values.
func configuredWith(t *testing.T, f *fakeTMDB, extra map[string]string) *provider {
	t.Helper()
	p := configured(t, f)
	v := map[string]string{keyToken: goodToken, keyBaseURL: f.URL + "/3"}
	for k, x := range extra {
		v[k] = x
	}
	resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: v})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure(%v) = %v, %v", extra, resp, err)
	}
	return p
}

func TestSizedURL(t *testing.T) {
	for _, c := range []struct{ path, size, want string }{
		{"/a.jpg", "", imageBase + "original/a.jpg"},
		{"/a.jpg", "original", imageBase + "original/a.jpg"},
		{"/a.jpg", "w1280", imageBase + "w1280/a.jpg"},
		{"", "w1280", ""},
	} {
		if got := sizedURL(c.path, c.size); got != c.want {
			t.Errorf("sizedURL(%q, %q) = %q, want %q", c.path, c.size, got, c.want)
		}
	}
}

func TestScaleDims(t *testing.T) {
	for _, c := range []struct {
		w, h         int
		size         string
		wantW, wantH int
	}{
		{3840, 2160, "", 3840, 2160},
		{3840, 2160, "original", 3840, 2160},
		{3840, 2160, "w1280", 1280, 720},
		{1000, 563, "w1280", 1000, 563}, // never upscaled
		{2000, 3000, "w780", 780, 1170},
		{1000, 1500, "h632", 421, 632},
		{0, 0, "w1280", 0, 0}, // unknown source size stays unknown
	} {
		if w, h := scaleDims(c.w, c.h, c.size); w != c.wantW || h != c.wantH {
			t.Errorf("scaleDims(%d, %d, %q) = %d x %d, want %d x %d", c.w, c.h, c.size, w, h, c.wantW, c.wantH)
		}
	}
}

// The default (no setting) keeps today's behavior: every image is downloaded at original size.
func TestImageSizeDefaultIsOriginal(t *testing.T) {
	p := configured(t, newFakeTMDB(t))
	if p.imageSizes() != (imageSizes{}) {
		t.Fatalf("default sizes = %+v, want the zero value (original)", p.imageSizes())
	}
	m, err := p.GetPerson(context.Background(), &pluginv1.GetPersonRequest{Id: "287", Languages: &pluginv1.Languages{Language: "en"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range m.GetImages() {
		if !strings.HasPrefix(im.GetUrl(), imageBase+"original/") {
			t.Errorf("default person image %q is not original size", im.GetUrl())
		}
	}
}

func TestImageSizesApplied(t *testing.T) {
	f := newFakeTMDB(t)
	p := configuredWith(t, f, map[string]string{keyBackdropSize: "w1280", keyPosterSize: "w780", keyProfileSize: "h632"})
	ctx := context.Background()
	en := &pluginv1.Languages{Language: "en"}

	person, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287", Languages: en})
	if err != nil || len(person.GetImages()) == 0 {
		t.Fatalf("GetPerson = %v, %v", person, err)
	}
	for _, im := range person.GetImages() {
		if !strings.HasPrefix(im.GetUrl(), imageBase+"h632/") {
			t.Errorf("person image %q, want h632", im.GetUrl())
		}
	}
	imgs, err := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "287"}, Languages: en})
	if err != nil || len(imgs.GetImages()) == 0 {
		t.Fatalf("GetImages = %v, %v", imgs, err)
	}
	for _, im := range imgs.GetImages() {
		if !strings.HasPrefix(im.GetUrl(), imageBase+"h632/") {
			t.Errorf("person GetImages %q, want h632", im.GetUrl())
		}
	}

	movie, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "3030", Languages: en})
	if err != nil {
		t.Fatal(err)
	}
	backdrops := 0
	for _, im := range movie.GetImages() {
		switch im.GetType() {
		case pluginv1.ImageType_IMAGE_TYPE_BACKDROP:
			backdrops++
			if !strings.HasPrefix(im.GetUrl(), imageBase+"w1280/") {
				t.Errorf("backdrop %q, want w1280", im.GetUrl())
			}
		case pluginv1.ImageType_IMAGE_TYPE_PRIMARY:
			if !strings.HasPrefix(im.GetUrl(), imageBase+"w780/") {
				t.Errorf("poster %q, want w780", im.GetUrl())
			}
		case pluginv1.ImageType_IMAGE_TYPE_LOGO:
			if !strings.HasPrefix(im.GetUrl(), imageBase+"original/") {
				t.Errorf("logo %q should stay original", im.GetUrl())
			}
		}
	}
	if backdrops == 0 {
		t.Fatal("no backdrops returned")
	}

	film, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550", Languages: en})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range film.GetCredits() {
		if c.GetImageUrl() != "" && !strings.HasPrefix(c.GetImageUrl(), imageBase+"h632/") {
			t.Errorf("credit photo %q, want h632", c.GetImageUrl())
		}
	}

	res, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Fight Club", Year: 1999})
	if err != nil || len(res.GetCandidates()) == 0 {
		t.Fatalf("Search = %v, %v", res, err)
	}
	for _, c := range res.GetCandidates() {
		if !strings.HasPrefix(c.GetImageUrl(), imageBase+"w780/") {
			t.Errorf("candidate poster %q, want w780", c.GetImageUrl())
		}
	}
}

// Changing a setting must not start any work: Configure makes no request except the token check.
func TestImageSizeSettingStartsNothing(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	before := len(f.seen())
	configuredAgain := map[string]string{keyToken: goodToken, keyBaseURL: f.URL + "/3", keyBackdropSize: "w1280"}
	if resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: configuredAgain}); err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure = %v, %v", resp, err)
	}
	for _, path := range f.seen()[before:] {
		if !strings.Contains(path, "/authentication") {
			t.Errorf("changing a size setting caused a request to %s", path)
		}
	}
}

func TestImageSizeValidationAndDescribe(t *testing.T) {
	p := newProvider()
	for _, bad := range []struct{ key, val string }{
		{keyBackdropSize, "w300"}, {keyBackdropSize, "w780"}, {keyPosterSize, "w500"}, {keyProfileSize, "w185"}, {keyProfileSize, "x"},
	} {
		resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, bad.key: bad.val}, Language: "en"})
		if err != nil || resp.GetFieldErrors()[bad.key] == "" {
			t.Errorf("%s=%q accepted: %v %v", bad.key, bad.val, resp, err)
		}
	}
	for _, lang := range []string{"en", "zh-TW", "zh-CN"} {
		m, err := p.Describe(context.Background(), &pluginv1.DescribeRequest{Language: lang})
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, f := range m.GetConfigFields() {
			switch f.GetKey() {
			case keyBackdropSize, keyPosterSize, keyProfileSize:
				found++
				if f.GetDefaultValue() != sizeOriginal || f.GetGroup() != "advanced" || len(f.GetOptions()) != 2 ||
					f.GetOptions()[0].GetValue() != sizeOriginal || f.GetLabel() == "" || f.GetDescription() == "" {
					t.Errorf("%s (%s): bad field %v", f.GetKey(), lang, f)
				}
			}
		}
		if found != 3 {
			t.Errorf("%s: %d size fields, want 3", lang, found)
		}
	}
}

// A hanging TMDB must not hold Configure up for the client's 20 s timeout: the token check is capped and the settings are still accepted.
func TestConfigureTokenCheckIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.http = srv.Client()
		c.http.Timeout = requestTimeout
		return c
	}
	start := time.Now()
	resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, keyBaseURL: srv.URL + "/3"}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure = %v, %v", resp, err)
	}
	if d := time.Since(start); d > authCheckTimeout+2*time.Second {
		t.Errorf("Configure took %v, want about %v at most", d, authCheckTimeout)
	}
}
