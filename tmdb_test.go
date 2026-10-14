package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	goodToken = "eyJhbGciOiJIUzI1NiJ9.SECRET-TOKEN.sig" // v4 Read Access Token (JWT shape)
	goodKey   = "0123456789abcdef0123456789abcdef"      // v3 API key
)

// fakeTMDB simulates a small part of the TMDB API v3 (returning only the fields the tests need).
type fakeTMDB struct {
	*httptest.Server
	requests atomic.Int64
	revoked  atomic.Bool
	flaky    atomic.Int64
	mu       sync.Mutex
	paths    []string // request paths and languages received
}

func (f *fakeTMDB) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func newFakeTMDB(t *testing.T) *fakeTMDB {
	f := &fakeTMDB{}
	tr := func(lang, region string, data map[string]any) map[string]any {
		return map[string]any{"iso_639_1": lang, "iso_3166_1": region, "data": data}
	}
	vid := func(key, site, typ, lang, region string, official bool, at string) map[string]any {
		var l, r any = lang, region
		if lang == "" {
			l, r = nil, nil
		}
		return map[string]any{"key": key, "site": site, "type": typ, "name": key, "official": official, "iso_639_1": l, "iso_3166_1": r, "published_at": at}
	}
	img := func(path, lang, region string, vote float64) map[string]any {
		var l, r any = lang, region
		if lang == "" {
			l = nil
		}
		if region == "" {
			r = nil
		}
		return map[string]any{"file_path": path, "iso_639_1": l, "iso_3166_1": r, "vote_average": vote, "width": 1000, "height": 1500}
	}
	// Whole-season response: episode names differ by language. zh-TW has only placeholders and zh-CN has a real episode name (to test the fallback and Simplified/Traditional conversion).
	episodeNames := map[string]string{"zh-TW": "第 1 集", "zh-HK": "第 1 集", "zh-CN": "凛冬将至", "en-US": "Winter Is Coming"}
	routes := map[string]func(q map[string]string) (int, any){
		"/3/authentication": func(map[string]string) (int, any) { return 200, map[string]any{"success": true} },
		"/3/movie/502": func(map[string]string) (int, any) { // 502 the first time, success afterwards
			if f.flaky.Add(1) == 1 {
				return 502, nil
			}
			return 200, map[string]any{"id": 502, "title": "Flaky"}
		},
		"/3/movie/503": func(map[string]string) (int, any) { return 503, nil },
		"/3/movie/3030": func(map[string]string) (int, any) { // a title with many stills: 30 backdrops (with mixed text languages)
			var bds []any
			for i := range 30 {
				lang := []string{"", "en", "ja"}[i%3]
				bds = append(bds, img(fmt.Sprintf("/still%02d.jpg", i), lang, "", float64(30-i)))
			}
			return 200, map[string]any{"id": 3030, "title": "Many Stills", "original_title": "Many Stills", "release_date": "2020-01-01",
				"images": map[string]any{"posters": []any{img("/p.jpg", "en", "US", 5)}, "backdrops": bds, "logos": []any{}}}
		},
		"/3/search/movie": func(q map[string]string) (int, any) {
			switch {
			case q["query"] == "busy":
				return 429, nil
			case q["query"] == "Fight Club" && q["year"] == "1999" && q["include_adult"] == "false":
				// TMDB's order is deliberately not the best: the plugin reorders by its own score.
				return 200, map[string]any{"results": []any{
					map[string]any{"id": 9, "title": "Club Fight", "release_date": "2010-01-01", "poster_path": "/c.jpg"},
					map[string]any{"id": 8, "title": "Fight Club: Members Only", "release_date": "2006-02-02", "poster_path": "/m.jpg"},
					map[string]any{"id": 550, "title": "Fight Club", "original_title": "Fight Club", "release_date": "1999-10-15", "poster_path": "/p.jpg"},
				}}
			case q["query"] == "斗阵俱乐部": // the Traditional query finds nothing; it is found only after converting to Simplified
				return 200, map[string]any{"results": []any{map[string]any{"id": 550, "title": "鬥陣俱樂部", "original_title": "Fight Club",
					"release_date": "1999-10-15", "poster_path": "/p.jpg"}}}
			}
			return 200, map[string]any{"results": []any{}}
		},
		"/3/search/tv": func(q map[string]string) (int, any) {
			if _, ok := q["first_air_date_year"]; ok {
				t.Errorf("tv search filtered by year: %v", q)
			}
			return 200, map[string]any{"results": []any{map[string]any{"id": 1399, "name": "Game of Thrones", "first_air_date": "2011-04-17"}}}
		},
		"/3/find/tt0137523": func(q map[string]string) (int, any) {
			return 200, map[string]any{"movie_results": []any{map[string]any{"id": 550, "title": "Fight Club"}}, "tv_results": []any{}}
		},
		"/3/find/121361": func(q map[string]string) (int, any) {
			if q["external_source"] != "tvdb_id" {
				return 404, nil
			}
			return 200, map[string]any{"movie_results": []any{}, "tv_results": []any{map[string]any{"id": 1399, "name": "Game of Thrones"}}}
		},
		"/3/movie/550": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 550, "title": "鬥陣俱樂部", "original_title": "Fight Club", "overview": "",
				"release_date": "1999-10-15", "runtime": 139, "vote_average": 8.4, "imdb_id": "tt0137523",
				"genres":                []any{map[string]any{"id": 18, "name": "剧情"}}, // TMDB's Chinese genre names are Simplified
				"production_companies":  []any{map[string]any{"name": "Fox 2000 Pictures"}},
				"production_countries":  []any{map[string]any{"iso_3166_1": "US", "name": "United States of America"}},
				"belongs_to_collection": nil,
				"credits": map[string]any{
					"cast": []any{map[string]any{"id": 819, "name": "Edward Norton", "character": "Narrator", "order": 0, "profile_path": "/en.jpg"}},
					"crew": []any{map[string]any{"id": 7467, "name": "David Fincher", "job": "Director", "department": "Directing"},
						map[string]any{"id": 7468, "name": "Jim Uhls", "job": "Screenplay", "department": "Writing"},
						map[string]any{"id": 1, "name": "Grip", "job": "Grip", "department": "Crew"}},
				},
				"images": map[string]any{
					"posters":   []any{img("/cn.jpg", "zh", "CN", 9), img("/en.jpg", "en", "US", 10), img("/tw.jpg", "zh", "TW", 1)},
					"backdrops": []any{img("/bd.jpg", "", "", 5)},
					"logos": []any{img("/logo-en.png", "en", "US", 10), img("/logo-tw.svg", "zh", "TW", 10),
						img("/logo-cn.png", "zh", "CN", 9), img("/logo-tw.png", "zh", "TW", 1), img("/logo-none.png", "", "", 8)},
				},
				"external_ids": map[string]any{"imdb_id": "tt0137523"},
				"videos": map[string]any{"results": []any{
					vid("en1", "YouTube", "Trailer", "en", "US", true, "2020-01-01T00:00:00.000Z"),
					vid("tw-teaser", "YouTube", "Teaser", "zh", "TW", false, "2021-01-01T00:00:00.000Z"),
					vid("tw-trailer", "YouTube", "Trailer", "zh", "TW", false, "2019-01-01T00:00:00.000Z"),
					vid("cn1", "YouTube", "Trailer", "zh", "CN", true, "2022-01-01T00:00:00.000Z"),
					vid("vimeo", "Vimeo", "Trailer", "zh", "TW", true, "2022-01-01T00:00:00.000Z"),
					vid("clip", "YouTube", "Clip", "zh", "TW", true, "2022-01-01T00:00:00.000Z"),
					vid("tw-trailer", "YouTube", "Trailer", "zh", "TW", false, "2019-01-01T00:00:00.000Z"), // duplicate
					vid("none", "YouTube", "Trailer", "", "", false, "2018-01-01T00:00:00.000Z"),
				}},
				"release_dates": map[string]any{"results": []any{
					map[string]any{"iso_3166_1": "TW", "release_dates": []any{
						map[string]any{"certification": "普遍級", "type": 1}, map[string]any{"certification": "限制級", "type": 3}}},
					map[string]any{"iso_3166_1": "US", "release_dates": []any{map[string]any{"certification": "R", "type": 3}}},
				}},
				"translations": map[string]any{"translations": []any{
					tr("zh", "TW", map[string]any{"title": "鬥陣俱樂部", "overview": ""}),
					tr("zh", "CN", map[string]any{"title": "搏击俱乐部", "overview": "一个失眠的上班族……"}),
					tr("en", "US", map[string]any{"title": "Fight Club", "overview": "A ticking-time-bomb insomniac..."}),
				}},
			}
		},
		// 1678: no Traditional Chinese title; the Taiwan title is registered only in alternative_titles (that is what the real TMDB data looks like).
		"/3/movie/1678": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 1678, "title": "ゴジラ", "original_title": "ゴジラ", "original_language": "ja", "release_date": "1954-11-03",
				"translations": map[string]any{"translations": []any{
					tr("zh", "TW", map[string]any{"title": "", "overview": "繁中簡介"}),
					tr("zh", "CN", map[string]any{"title": "哥斯拉", "overview": "简中简介"}),
					tr("en", "US", map[string]any{"title": "Godzilla", "overview": "en"}),
				}},
				"alternative_titles": map[string]any{"titles": []any{
					map[string]any{"iso_3166_1": "US", "title": "Godzilla, King of the Monsters!"},
					map[string]any{"iso_3166_1": "TW", "title": "哥吉拉"},
				}},
			}
		},
		// Images in the original language (change order 0116 E4): include_image_language carries only the original language.
		"/3/movie/1678/images": func(q map[string]string) (int, any) {
			if q["include_image_language"] != "ja" {
				t.Errorf("original-language images requested with include_image_language=%q", q["include_image_language"])
			}
			return 200, map[string]any{"posters": []any{img("/ja1.jpg", "ja", "JP", 7), img("/ja2.jpg", "ja", "JP", 6)}, "backdrops": []any{img("/jab.jpg", "ja", "", 5)},
				"logos": []any{}}
		},
		// 2: only a Traditional Chinese translation; a Simplified Chinese target has to convert.
		"/3/movie/2": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 2, "title": "x", "original_title": "Original",
				"translations": map[string]any{"translations": []any{
					tr("zh", "TW", map[string]any{"title": "軟體戰爭", "overview": "繁體簡介"}),
				}},
			}
		},
		"/3/tv/1399": func(q map[string]string) (int, any) {
			if q["append_to_response"] != "" && !strings.Contains(q["append_to_response"], "aggregate_credits") {
				t.Errorf("series details without aggregate_credits: %s", q["append_to_response"])
			}
			return 200, map[string]any{"id": 1399, "name": "Game of Thrones", "overview": "o", "first_air_date": "2011-04-17",
				"last_air_date": "2019-05-19", "status": q["status"], "original_language": "en",
				"aggregate_credits": map[string]any{
					"cast": []any{
						map[string]any{"id": 1, "name": "Minor", "order": 0, "total_episode_count": 2,
							"roles": []any{map[string]any{"character": "Guard", "episode_count": 2}}},
						map[string]any{"id": 22970, "name": "Peter Dinklage", "order": 1, "total_episode_count": 67,
							"roles": []any{map[string]any{"character": "Tyrion's double", "episode_count": 1},
								map[string]any{"character": "Tyrion Lannister", "episode_count": 66}}},
					},
					"crew": []any{map[string]any{"id": 9813, "name": "David Benioff", "department": "Writing",
						"jobs": []any{map[string]any{"job": "Writer"}, map[string]any{"job": "Executive Producer"}}}},
				}}
		},
		"/3/tv/1399/season/1": func(q map[string]string) (int, any) {
			lang := q["language"]
			s := map[string]any{"id": 3624, "name": map[bool]string{true: "第 1 季", false: "Season 1"}[strings.HasPrefix(lang, "zh")],
				"air_date": "2011-04-17", "season_number": 1, "poster_path": "/s1.jpg",
				"episodes": []any{map[string]any{"id": 63056, "name": first(episodeNames[lang], "Episode 1"),
					"overview": map[bool]string{true: "x", false: ""}[lang == "en-US"], "air_date": "2011-04-17",
					"season_number": 1, "episode_number": 1, "still_path": "/still.jpg", "runtime": 62,
					"crew":        []any{map[string]any{"id": 44797, "name": "Tim Van Patten", "job": "Director", "department": "Directing"}},
					"guest_stars": []any{map[string]any{"id": 5, "name": "Guest", "character": "Someone"}, map[string]any{"id": 5, "name": "Guest", "character": "Someone"}},
				}},
			}
			if q["append_to_response"] != "" {
				s["translations"] = map[string]any{"translations": []any{
					tr("zh", "TW", map[string]any{"name": "第 1 季", "overview": "第一季簡介"}),
					tr("zh", "HK", map[string]any{"name": "权力的游戏"}), // wrongly filled in as the series title (real TMDB data has this)
					tr("en", "US", map[string]any{"name": "The Real Season Name"}),
				}}
				s["images"] = map[string]any{"posters": []any{img("/s1-en.jpg", "en", "", 1), img("/s1-tw.jpg", "zh", "TW", 1)}}
			}
			return 200, s
		},
		"/3/tv/1399/translations": func(q map[string]string) (int, any) {
			return 200, map[string]any{"translations": []any{tr("zh", "CN", map[string]any{"name": "權力的遊戲"}),
				tr("en", "US", map[string]any{"name": "Game of Thrones"})}}
		},
		"/3/tv/2/season/1": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 9, "season_number": 1, "episodes": []any{},
				"translations": map[string]any{"translations": []any{tr("zh", "CN", map[string]any{"name": "无限列车篇"})}}}
		},
		"/3/person/287": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 287, "name": "Brad Pitt", "biography": "bio",
				"also_known_as": []any{"ブラッド・ピット", "布拉德·皮特", "布萊德·彼特"},
				"birthday":      "1963-12-18", "deathday": nil, "place_of_birth": "Shawnee, Oklahoma, USA", "gender": 2,
				"imdb_id": "nm0000093", "profile_path": "/bp.jpg",
				"external_ids": map[string]any{"instagram_id": "bradpitt", "wikidata_id": "Q35332"},
				"translations": map[string]any{"translations": []any{
					tr("en", "US", map[string]any{"name": "", "biography": "bio"}),
					tr("zh", "CN", map[string]any{"name": "", "biography": "美国演员"}),
				}},
			}
		},
		"/3/person/287/images": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 287, "profiles": []any{
				img("/low.jpg", "", "", 3), img("/best.jpg", "", "", 9), map[string]any{"file_path": "/vec.svg", "vote_average": 10.0, "width": 10, "height": 10},
				img("/mid.jpg", "", "", 5)}}
		},
		"/3/person/1": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 1, "name": "Someone", "translations": map[string]any{"translations": []any{
				tr("zh", "CN", map[string]any{"name": "张艺谋"}),
			}}}
		},
		"/3/collection/10": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 10, "name": "Star Wars Collection", "overview": "o",
				"parts": []any{map[string]any{"id": 2, "title": "B", "release_date": "1980-05-20"},
					map[string]any{"id": 3, "title": "Unreleased", "release_date": ""},
					map[string]any{"id": 1, "title": "A", "release_date": "1977-05-25"}},
				"translations": map[string]any{"translations": []any{
					tr("zh", "CN", map[string]any{"title": "星球大战（系列）", "overview": "简介"}),
				}},
			}
		},
		// Person lookup by name (change order 0079): Alice has two people with that name, and 501's known_for contains movie 550; Bob's 601 is confirmed only through combined_credits.
		"/3/search/person": func(q map[string]string) (int, any) {
			switch q["query"] {
			case "Alice Example":
				return 200, map[string]any{"results": []any{
					map[string]any{"id": 502, "name": "Alice Example", "gender": 1, "known_for": []any{map[string]any{"id": 77, "media_type": "movie"}}},
					map[string]any{"id": 501, "name": "Alice Example", "gender": 1, "profile_path": "/a.jpg",
						"known_for": []any{map[string]any{"id": 550, "media_type": "movie"}}},
				}}
			case "Bob Example":
				return 200, map[string]any{"results": []any{
					map[string]any{"id": 601, "name": "Bob Example", "gender": 2, "known_for": []any{}},
					map[string]any{"id": 602, "name": "Bob Example", "gender": 2, "known_for": []any{}},
				}}
			}
			return 200, map[string]any{"results": []any{}}
		},
		"/3/person/601/combined_credits": func(q map[string]string) (int, any) {
			return 200, map[string]any{"cast": []any{map[string]any{"id": 550, "media_type": "movie"}}, "crew": []any{}}
		},
		"/3/person/602/combined_credits": func(q map[string]string) (int, any) {
			return 200, map[string]any{"cast": []any{map[string]any{"id": 550, "media_type": "tv"}}, "crew": []any{}}
		},
		"/3/person/502/combined_credits": func(q map[string]string) (int, any) {
			return 200, map[string]any{"cast": []any{}, "crew": []any{map[string]any{"id": 5, "media_type": "movie"}}}
		},
		// Both the translated title and the overview are empty strings (TMDB often has translations where only other fields are filled): fall back to TMDB's original text; an empty name must not be returned.
		"/3/collection/11": func(q map[string]string) (int, any) {
			return 200, map[string]any{"id": 11, "name": "Plain Saga", "overview": "plain overview", "parts": []any{},
				"translations": map[string]any{"translations": []any{
					tr("zh", "TW", map[string]any{"title": "", "overview": ""}),
					tr("zh", "CN", map[string]any{"title": " ", "overview": ""}),
				}},
			}
		},
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		bearer := r.Header.Get("Authorization") == "Bearer "+goodToken
		apiKey := r.URL.Query().Get("api_key") == goodKey && r.Header.Get("Authorization") == ""
		if !bearer && !apiKey || f.revoked.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.RawQuery, "SECRET") {
			t.Errorf("bearer token in query string: %s", r.URL.RawQuery)
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path+"?language="+r.URL.Query().Get("language")+"&append="+r.URL.Query().Get("append_to_response"))
		f.mu.Unlock()
		h, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		code, body := h(q)
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		w.WriteHeader(code)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func configured(t *testing.T, f *fakeTMDB) *provider {
	t.Helper()
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.http = f.Client()
		c.limiter = 0 // the test is not affected by self rate limiting (see TestRateLimiter)
		c.retry = time.Millisecond
		return c
	}
	resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{
		keyToken: goodToken, keyBaseURL: f.URL + "/3"}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure = %v, %v", resp, err)
	}
	return p
}

func code(err error) codes.Code { return status.Code(err) }

func TestDescribe(t *testing.T) {
	m, _ := newProvider().Describe(context.Background(), &pluginv1.DescribeRequest{Language: "zh-TW"})
	if m.GetId() != "tmdb" || len(m.GetCapabilities()) != len(capabilities()) || m.GetAttribution().GetText() == "" ||
		!strings.Contains(m.GetDescription(), "電影") || m.GetSuggestedConcurrency() != 12 {
		t.Errorf("manifest = %v", m)
	}
	// The plan table (change order 0144): TMDB is a free official plugin, so it must not declare a licensed feature.
	if len(m.GetFeatures()) != 0 {
		t.Errorf("tmdb declares features %v, it is free", m.GetFeatures())
	}
}

// TestAttributionOnce: TMDB's notice (the English original) is only in attribution.text, and the Manifest.description of each language does not repeat it.
func TestAttributionOnce(t *testing.T) {
	const want = "This product uses the TMDB API but is not endorsed or certified by TMDB."
	for _, lang := range []string{"en", "zh-TW", "zh-CN", "", "ja"} {
		m, _ := newProvider().Describe(context.Background(), &pluginv1.DescribeRequest{Language: lang})
		if strings.Contains(m.GetDescription(), "endorsed") || m.GetAttribution().GetText() != want {
			t.Errorf("%q: description %q, attribution %q", lang, m.GetDescription(), m.GetAttribution().GetText())
		}
	}
}

// TestAttributionLogo: the logo is the original file of the official TMDB SVG (unmodified), placed in attribution.logo_url as a base64 data URI.
func TestAttributionLogo(t *testing.T) {
	m, _ := newProvider().Describe(context.Background(), &pluginv1.DescribeRequest{})
	const prefix = "data:image/svg+xml;base64,"
	u := m.GetAttribution().GetLogoUrl()
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("logo_url = %.40q", u)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u, prefix))
	if err != nil || !bytes.HasPrefix(raw, []byte("<svg ")) || !bytes.Contains(raw, []byte(`viewBox="0 0 273.42 35.52"`)) || len(u) > 32<<10 {
		t.Errorf("logo svg: %v, %d bytes", err, len(raw))
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != "8e7b30f73a4020692ccca9c88bafe5dcb6f8a62a4c6bc55cd9ba82bb2cd95f6c" {
		t.Errorf("logo file modified: sha256 %s", got)
	}
}

// TestVersionFile: the Manifest version comes from VERSION (make plugins uses the same file to decide the directory name), and it must be a semantic version.
func TestVersionFile(t *testing.T) {
	m, _ := newProvider().Describe(context.Background(), &pluginv1.DescribeRequest{})
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(m.GetVersion()) || m.GetVersion() != strings.TrimSpace(versionFile) {
		t.Errorf("version = %q", m.GetVersion())
	}
}

func TestConfigure(t *testing.T) {
	f := newFakeTMDB(t)
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.http = f.Client()
		return c
	}
	ctx := context.Background()
	tests := map[string]struct {
		values map[string]string
		field  string
	}{
		"missing token": {map[string]string{keyBaseURL: f.URL + "/3"}, keyToken},
		"wrong token":   {map[string]string{keyToken: "nope", keyBaseURL: f.URL + "/3"}, keyToken},
		"http base":     {map[string]string{keyToken: goodToken, keyBaseURL: "http://example.com/3"}, keyBaseURL},
	}
	for name, tc := range tests {
		resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: tc.values})
		if err != nil || resp.GetFieldErrors()[tc.field] == "" {
			t.Errorf("%s: %v %v", name, resp, err)
		}
	}
	if _, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "x"}); code(err) != codes.FailedPrecondition {
		t.Errorf("search before configuration: %v", err)
	}
	// Settings are still accepted when TMDB cannot be reached (the network may not be ready at boot).
	resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, keyBaseURL: "https://127.0.0.1:1/3"}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Errorf("unreachable TMDB: %v %v", resp, err)
	}
}

func zhTW(fallbacks ...string) *pluginv1.Languages {
	return &pluginv1.Languages{Language: "zh-TW", Fallbacks: fallbacks}
}

func TestSearch(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	r, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Fight Club", Year: 1999})
	if err != nil || len(r.GetCandidates()) != 3 {
		t.Fatalf("search: %v %v", r, err)
	}
	// Reorder by the plugin's score: identical 1, prefix with a large year difference 0.85-0.2, only the words equal with a large year difference 0.6-0.2.
	var ids []string
	var scores []float64
	for _, c := range r.GetCandidates() {
		ids = append(ids, c.GetId())
		scores = append(scores, math.Round(c.GetScore()*100)/100)
	}
	if strings.Join(ids, ",") != "550,8,9" || scores[0] != 1 || scores[1] != 0.65 || scores[2] != 0.4 {
		t.Errorf("order %v scores %v", ids, scores)
	}
	c := r.GetCandidates()[0]
	if c.GetYear() != 1999 || c.GetImageUrl() != imageBaseURL+"/p.jpg" || c.GetProviderIds()["tmdb"] != "550" {
		t.Errorf("candidate = %v", c)
	}
	limited, _ := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Fight Club", Year: 1999, Limit: 1})
	if len(limited.GetCandidates()) != 1 || limited.GetCandidates()[0].GetId() != "550" {
		t.Errorf("limit 1: %v", limited)
	}
	// imdb and tvdb hints: located directly with /find, score 1, no name search.
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "whatever",
		ProviderIds: map[string]string{"imdb": "tt0137523"}})
	if err != nil || len(r.GetCandidates()) != 1 || r.GetCandidates()[0].GetId() != "550" || r.GetCandidates()[0].GetScore() != 1 {
		t.Errorf("imdb hint: %v %v", r, err)
	}
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Name: "whatever",
		ProviderIds: map[string]string{"tvdb": "121361"}})
	if err != nil || len(r.GetCandidates()) != 1 || r.GetCandidates()[0].GetId() != "1399" {
		t.Errorf("tvdb hint: %v %v", r, err)
	}
	// Series search does not filter by year (the fake TMDB reports an error if it receives first_air_date_year); the year affects only the score: 1 - 0.05 (one year off) - 0.01 (no poster).
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Name: "Game of Thrones", Year: 2012})
	if err != nil || len(r.GetCandidates()) != 1 || math.Abs(r.GetCandidates()[0].GetScore()-0.94) > 1e-9 {
		t.Errorf("tv search: %v %v", r, err)
	}
	if _, err := p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SEASON}); code(err) != codes.InvalidArgument {
		t.Errorf("season search: %v", err)
	}
	// 429: returns RateLimitError, carrying TMDB's Retry-After.
	_, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "busy"})
	var rl *sdk.RateLimitError
	if !asRateLimit(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Errorf("429: %v", err)
	}
}

// TestSearchRetriesOtherScript: when a Traditional title finds nothing, search once more with the title converted to Simplified.
func TestSearchRetriesOtherScript(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	r, err := p.Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "鬥陣俱樂部",
		Languages: zhTW()})
	if err != nil || len(r.GetCandidates()) != 1 || r.GetCandidates()[0].GetId() != "550" || r.GetCandidates()[0].GetScore() != 1 {
		t.Errorf("search: %v %v", r, err)
	}
}

func TestTitleScore(t *testing.T) {
	p := newProvider()
	for _, tc := range []struct {
		q, title string
		want     float64
	}{
		{"Fight Club", "fight club", 1},
		{"Ｆｉｇｈｔ　Ｃｌｕｂ", "Fight Club", 1}, // full-width
		{"鬥陣俱樂部", "斗阵俱乐部", 1},           // Simplified and Traditional count as the same
		{"Fight Club", "Fight Club: Members Only", 0.85},
		{"Fight Club: Members Only", "Fight Club", 0.95}, // the query has extra text beyond the candidate: the candidate is a prefix of the query
		{"Club", "Fight Club", 0.8},
		{"Fight Club", "Club Fight", 0.6},
		{"abc", "xyz", 0},
	} {
		if got := p.titleScore(tc.q, tc.title); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("titleScore(%q, %q) = %v, want %v", tc.q, tc.title, got, tc.want)
		}
	}
	if s := p.score("X", 2000, tmdbSearchResult{Title: "X", ReleaseDate: "2001-01-01", PosterPath: "/p"}); math.Abs(s-0.95) > 1e-9 {
		t.Errorf("year ±1: %v", s)
	}
	if s := p.score("X", 2000, tmdbSearchResult{Title: "X"}); math.Abs(s-0.89) > 1e-9 {
		t.Errorf("no year, no poster: %v", s)
	}
}

func asRateLimit(err error, out **sdk.RateLimitError) bool { return errors.As(err, out) }

func TestGetMovie(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550",
		Languages: zhTW("zh-CN")})
	if err != nil {
		t.Fatal(err)
	}
	var posters []string
	for _, i := range m.GetImages() {
		if i.GetType() == pluginv1.ImageType_IMAGE_TYPE_PRIMARY {
			posters = append(posters, i.GetLanguage())
		}
	}
	checks := map[string]bool{
		"zh-TW title":                  m.GetName() == "鬥陣俱樂部",
		"overview from zh-CN, to Hant": m.GetOverview() == "一個失眠的上班族……",
		"genre from the table":         len(m.GetGenres()) == 1 && m.GetGenres()[0] == "劇情",
		"dates":                        m.GetPremiereDate() == "1999-10-15" && m.GetProductionYear() == 1999,
		"runtime":                      m.GetRuntimeSeconds() == 139*60,
		"TW theatrical rating":         m.GetOfficialRating() == "限制級",
		"provider ids":                 m.GetProviderIds()["tmdb"] == "550" && m.GetProviderIds()["imdb"] == "tt0137523",
		"studios and countries":        len(m.GetStudios()) == 1 && len(m.GetProductionLocations()) == 1,
		"posters by region":            strings.Join(posters, ",") == "zh-TW,zh-CN,en-US",
		"credits":                      len(m.GetCredits()) == 3, // cast, director, writer; Grip is skipped
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s: %v", name, m)
		}
	}
	if got := f.seen(); len(got) != 2 { // authentication and one detail request
		t.Errorf("requests = %v", got)
	}
	if _, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "999"}); code(err) != codes.NotFound {
		t.Errorf("unknown id: %v", err)
	}
}

// TestTitleFallback: when the title in the target language is missing, TMDB returns the original title; the plugin has to look at translations and regional aliases.
func TestTitleFallback(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	get := func(id, lang string) *pluginv1.ItemMetadata {
		t.Helper()
		m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: id,
			Languages: &pluginv1.Languages{Language: lang}})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, tc := range []struct{ id, lang, name, overview string }{
		{"1678", "zh-TW", "哥吉拉", "繁中簡介"}, // Taiwan alias
		{"1678", "zh-CN", "哥斯拉", "简中简介"}, // translation in the target language
		{"1678", "zh-HK", "哥斯拉", "繁中簡介"}, // no Hong Kong translation: the TW overview in the same script; the title uses CN converted to Traditional
		{"1678", "en-US", "Godzilla", "en"},
		{"1678", "ja", "ゴジラ", "en"},    // no Japanese translation: the original title; the overview goes along the fallback chain to English
		{"2", "zh-CN", "软件战争", "繁体简介"}, // only Traditional Chinese: converted to Simplified (including wording)
		{"2", "en", "Original", ""},
	} {
		m := get(tc.id, tc.lang)
		if m.GetName() != tc.name || m.GetOverview() != tc.overview {
			t.Errorf("%s %s: name %q overview %q, want %q %q", tc.id, tc.lang, m.GetName(), m.GetOverview(), tc.name, tc.overview)
		}
	}
	// Simplified/Traditional conversion off: no conversion, the original script is used.
	p.conv.setOff(true)
	if m := get("2", "zh-CN"); m.GetName() != "軟體戰爭" {
		t.Errorf("conversion off: %q", m.GetName())
	}
}

func TestGetSeasonAndEpisode(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	s, e := int32(1), int32(1)
	season, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_SEASON, SeriesId: "1399", SeasonNumber: &s,
		Languages: zhTW()})
	if err != nil {
		t.Fatal(err)
	}
	if season.GetName() != "第 1 季" || season.GetOverview() != "第一季簡介" || season.GetIndexNumber() != 1 ||
		len(season.GetImages()) != 2 || season.GetImages()[0].GetLanguage() != "zh-TW" {
		t.Errorf("season = %v", season)
	}
	// A genuine season name: one from another region of the same language is also used (converted to the target script); failing to get the series translations (404) does not matter.
	if named, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_SEASON, SeriesId: "2", SeasonNumber: &s,
		Languages: zhTW()}); err != nil || named.GetName() != "無限列車篇" {
		t.Errorf("named season: %v %v", named, err)
	}
	before := len(f.seen())
	m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399",
		SeasonNumber: &s, EpisodeNumber: &e, Languages: zhTW()})
	if err != nil {
		t.Fatal(err)
	}
	// The episode names of zh-TW and zh-HK are placeholders, zh-MO has none, zh-CN has one: converted to Traditional. The overview is English only.
	if m.GetName() != "凜冬將至" || m.GetOverview() != "x" || m.GetIndexNumber() != 1 || m.GetParentIndexNumber() != 1 ||
		len(m.GetImages()) != 1 || m.GetImages()[0].GetUrl() != imageBaseURL+"/still.jpg" {
		t.Errorf("episode = %v", m)
	}
	kinds := map[pluginv1.PersonKind]int{}
	for _, c := range m.GetCredits() {
		kinds[c.GetKind()]++
	}
	if kinds[pluginv1.PersonKind_PERSON_KIND_GUEST_STAR] != 1 || kinds[pluginv1.PersonKind_PERSON_KIND_DIRECTOR] != 1 {
		t.Errorf("episode credits = %v", m.GetCredits())
	}
	// The whole season is already in the cache (from the season request): only the requests for the fallback languages.
	got := f.seen()[before:]
	for _, r := range got {
		if strings.Contains(r, "language=zh-TW") || strings.Contains(r, "append=images") {
			t.Errorf("target season fetched again: %v", got)
		}
	}
	// Fetch again: everything comes from the cache.
	n := len(f.seen())
	if _, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399",
		SeasonNumber: &s, EpisodeNumber: &e, Languages: zhTW()}); err != nil {
		t.Fatal(err)
	}
	if extra := f.seen()[n:]; len(extra) != 0 {
		t.Errorf("cached episode made requests: %v", extra)
	}
	missing := int32(99)
	if _, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399",
		SeasonNumber: &s, EpisodeNumber: &missing}); code(err) != codes.NotFound {
		t.Errorf("missing episode: %v", err)
	}
	if _, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399"}); code(err) != codes.InvalidArgument {
		t.Errorf("episode without numbers: %v", err)
	}
}

// TestEpisodePlaceholderOnly: when every language has only placeholders, the episode name is left empty (the server shows the "Episode N" form).
func TestEpisodePlaceholderOnly(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	s, e := int32(1), int32(1)
	m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399",
		SeasonNumber: &s, EpisodeNumber: &e, Languages: &pluginv1.Languages{Language: "ja"}})
	if err != nil {
		t.Fatal(err)
	}
	// ja is the placeholder "Episode 1", the fallback has only en-US, and the name is the genuine English episode name.
	if m.GetName() != "Winter Is Coming" {
		t.Errorf("name = %q", m.GetName())
	}
	if !isPlaceholder("Episode 1") || !isPlaceholder("第 3 集") || !isPlaceholder("第三話") || !isPlaceholder("12") ||
		isPlaceholder("Winter Is Coming") || isPlaceholder("第一滴血") {
		t.Error("isPlaceholder")
	}
}

func TestSeasonNames(t *testing.T) {
	for _, tc := range []struct {
		lang string
		n    int
		want string
	}{{"zh-TW", 0, "特別篇"}, {"zh-CN", 0, "特别篇"}, {"zh-HK", 2, "第 2 季"}, {"en", 0, "Specials"}, {"fr", 3, "Season 3"}} {
		if got := seasonName(parseLocale(tc.lang), tc.n); got != tc.want {
			t.Errorf("seasonName(%s, %d) = %q", tc.lang, tc.n, got)
		}
	}
}

func TestPersonAndCollection(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	pe, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287", Languages: zhTW()})
	if err != nil {
		t.Fatal(err)
	}
	if pe.GetGender() != pluginv1.Gender_GENDER_MALE || pe.GetBirthDate() != "1963-12-18" || pe.GetDeathDate() != "" ||
		pe.GetProviderIds()["imdb"] != "nm0000093" || len(pe.GetLinks()) != 3 || len(pe.GetImages()) != 1 {
		t.Errorf("person = %v", pe)
	}
	// No Chinese translation: a Traditional target uses an alias known to be Traditional; the overview is converted from Simplified to Traditional.
	if pe.GetName() != "布萊德·彼特" || pe.GetOverview() != "美國演員" {
		t.Errorf("zh-TW person: %q %q", pe.GetName(), pe.GetOverview())
	}
	cn, _ := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287", Languages: &pluginv1.Languages{Language: "zh-CN"}})
	if cn.GetName() != "布拉德·皮特" || cn.GetOverview() != "美国演员" {
		t.Errorf("zh-CN person: %q %q", cn.GetName(), cn.GetOverview())
	}
	en, _ := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287"})
	if en.GetName() != "Brad Pitt" || en.GetOverview() != "bio" {
		t.Errorf("en person: %q %q", en.GetName(), en.GetOverview())
	}
	// Translation from another region of the same language: converted to the target script.
	if one, _ := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "1", Languages: zhTW()}); one.GetName() != "張藝謀" {
		t.Errorf("translated name: %q", one.GetName())
	}
	col, err := p.GetCollection(ctx, &pluginv1.GetCollectionRequest{Id: "10", Languages: zhTW()})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, part := range col.GetParts() {
		order = append(order, part.GetName())
	}
	if strings.Join(order, ",") != "A,B,Unreleased" || col.GetName() != "星球大戰（系列）" {
		t.Errorf("collection %q parts %v", col.GetName(), order)
	}
}

func TestConfigureSettings(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	base := map[string]string{keyToken: goodToken, keyBaseURL: f.URL + "/3"}
	with := func(k, v string) map[string]string {
		m := maps.Clone(base)
		m[k] = v
		return m
	}
	for _, bad := range []map[string]string{with(keyMaxCast, "abc"), with(keyMaxCast, "-1"), with(keyMaxCast, "501"), with(keyCertCountry, "USA")} {
		if resp, _ := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: bad}); len(resp.GetFieldErrors()) != 1 {
			t.Errorf("%v accepted: %v", bad, resp)
		}
	}
	v := with(keyMaxCast, "1")
	v[keyCertCountry] = "us"
	v[keyConvertZh] = "false"
	if resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: v}); err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("configure: %v %v", resp, err)
	}
	if p.castLimit() != 1 || p.ratingCountry(parseLocale("zh-TW")) != "US" || !p.conv.off {
		t.Errorf("settings not applied: cast %d country %s off %v", p.castLimit(), p.ratingCountry(parseLocale("zh-TW")), p.conv.off)
	}
	m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550", Languages: zhTW()})
	if err != nil || m.GetOfficialRating() != "R" {
		t.Errorf("certification country: %v %v", m.GetOfficialRating(), err)
	}
}

func TestLanguageHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"": "en-US", "zh-TW": "zh-TW", "zh_tw": "zh-TW", "ja": "ja", "zh-Hant-TW": "zh-TW",
		"zh-Hant": "zh-TW", "zh-Hans": "zh-CN", "zh": "zh-CN", "zh-Hant-HK": "zh-HK", "zh-hant": "zh-TW", "pt-BR": "pt-BR",
	} {
		if got := tmdbLanguage(in); got != want {
			t.Errorf("tmdbLanguage(%q) = %q, want %q", in, got, want)
		}
	}
	str := func(ch []locale) string {
		var out []string
		for _, l := range ch {
			out = append(out, l.String())
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		l    *pluginv1.Languages
		want string
	}{
		{zhTW("zh-CN", "zh-TW"), "zh-TW,zh-HK,zh-MO,zh-CN,zh-SG,en-US"},
		{&pluginv1.Languages{Language: "zh-HK"}, "zh-HK,zh-MO,zh-TW,zh-CN,zh-SG,en-US"},
		{&pluginv1.Languages{Language: "zh-Hans", Fallbacks: []string{"ja"}}, "zh-CN,zh-SG,zh-TW,zh-HK,ja,en-US"},
		{&pluginv1.Languages{Language: "fr-FR", Fallbacks: []string{"de"}}, "fr-FR,de,en-US"},
	} {
		if got := str(chain(tc.l)); got != tc.want {
			t.Errorf("chain(%v) = %s, want %s", tc.l, got, tc.want)
		}
	}
	if got := imageLanguages(chain(zhTW("ja"))); got != "zh,ja,en,xx,null" {
		t.Errorf("imageLanguages = %s", got)
	}
}

func TestChineseConversion(t *testing.T) {
	var c converter
	tw, hk, cn := parseLocale("zh-TW"), parseLocale("zh-HK"), parseLocale("zh-CN")
	for _, tc := range []struct {
		in       string
		from, to locale
		want     string
	}{
		{"软件", cn, tw, "軟體"}, // includes Taiwan wording
		{"软件", cn, hk, "軟件"},
		{"軟體", tw, cn, "软件"},
		{"軟件", hk, cn, "软件"},
		{"軟體", tw, hk, "軟體"}, // same script: no conversion
		{"Drama", cn, tw, "Drama"},
	} {
		if got := c.convert(tc.in, tc.from, tc.to); got != tc.want {
			t.Errorf("convert(%q, %s→%s) = %q, want %q", tc.in, tc.from, tc.to, got, tc.want)
		}
	}
	c.setOff(true)
	if got := c.convert("软件", cn, tw); got != "软件" {
		t.Errorf("off: %q", got)
	}
}

func TestGenreNames(t *testing.T) {
	p := newProvider()
	for _, tc := range []struct {
		id         int
		fallback   string
		lang, want string
	}{
		{18, "剧情", "zh-TW", "劇情"}, {18, "剧情", "zh-HK", "劇情"}, {18, "剧情", "zh-CN", "剧情"}, {18, "Drama", "en-US", "Drama"},
		{18, "ドラマ", "ja", "ドラマ"}, {10764, "真人秀", "zh-TW", "實境"}, {99999, "动漫", "zh-TW", "動漫"},
	} {
		if got := p.genreName(tc.id, tc.fallback, parseLocale(tc.lang)); got != tc.want {
			t.Errorf("genreName(%d, %s) = %q, want %q", tc.id, tc.lang, got, tc.want)
		}
	}
	// The table is complete: every ID has en, zh-CN and zh-TW.
	for id, names := range genreNames {
		for _, k := range []string{"en", "zh-CN", "zh-TW"} {
			if names[k] == "" {
				t.Errorf("genre %d lacks %s", id, k)
			}
		}
	}
}

func TestImageOrderFollowsLanguageAndRegion(t *testing.T) {
	imgs := []tmdbImage{
		{FilePath: "/en.jpg", Lang: "en", Region: "US", Vote: 9},
		{FilePath: "/none.jpg", Lang: "", Vote: 8},
		{FilePath: "/xx.jpg", Lang: "xx", Vote: 7},
		{FilePath: "/ja.jpg", Lang: "ja", Vote: 10},
		{FilePath: "/cn.jpg", Lang: "zh", Region: "CN", Vote: 10},
		{FilePath: "/zh.jpg", Lang: "zh", Vote: 5},
		{FilePath: "/hk.jpg", Lang: "zh", Region: "HK", Vote: 6},
		{FilePath: "/tw.jpg", Lang: "zh", Region: "TW", Vote: 1},
		{FilePath: "/logo.svg", Lang: "zh", Region: "TW", Vote: 10},
	}
	order := func(xs []tmdbImage) string {
		var out []string
		for _, x := range xs {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(x.FilePath, "/"), ".jpg"))
		}
		return strings.Join(out, ",")
	}
	tw := chain(zhTW())
	if got := order(sortImages(imgs, tw, true)); got != "tw,hk,zh,cn,en,none,xx,ja" {
		t.Errorf("zh-TW posters = %s", got)
	}
	if got := order(sortImages(imgs, tw, false)); got != "none,xx,tw,hk,zh,cn,en,ja" {
		t.Errorf("zh-TW backdrops = %s", got)
	}
	if got := order(sortImages(imgs, chain(&pluginv1.Languages{Language: "zh-CN"}), true)); got != "cn,zh,hk,tw,en,none,xx,ja" {
		t.Errorf("zh-CN posters = %s", got)
	}
}

func TestSeriesEndDateAndAggregateCredits(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	// The fake TMDB decides the returned status from the status query parameter; the plugin does not send this parameter, so an empty string is returned here (equivalent to still airing).
	m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Id: "1399"})
	if err != nil {
		t.Fatal(err)
	}
	if m.GetEndDate() != "" {
		t.Errorf("airing series got EndDate %q", m.GetEndDate())
	}
	kinds := map[pluginv1.PersonKind]int{}
	for _, c := range m.GetCredits() {
		kinds[c.GetKind()]++
	}
	// Cast ordered by episode appearances, with the role taken from the one with the most appearances; one person who is both writer and producer.
	if kinds[pluginv1.PersonKind_PERSON_KIND_ACTOR] != 2 || kinds[pluginv1.PersonKind_PERSON_KIND_WRITER] != 1 ||
		kinds[pluginv1.PersonKind_PERSON_KIND_PRODUCER] != 1 ||
		m.GetCredits()[0].GetName() != "Peter Dinklage" || m.GetCredits()[0].GetRole() != "Tyrion Lannister" {
		t.Errorf("credits = %v", m.GetCredits())
	}
	ended := tmdbItem{Name: "x", LastAirDate: "2019-05-19", Status: "Ended"}
	if got := p.titleMetadata(pluginv1.ItemType_ITEM_TYPE_SERIES, ended, chain(nil)).GetEndDate(); got != "2019-05-19" {
		t.Errorf("ended series EndDate = %q", got)
	}
}

func TestCreditsLimits(t *testing.T) {
	var cast []tmdbCast
	var crew []tmdbCrew
	for i := range 10 {
		cast = append(cast, tmdbCast{ID: i + 1, Name: "A"})
		crew = append(crew, tmdbCrew{ID: 100 + i, Name: "P", Job: "Producer", Department: "Production"})
	}
	crew = append(crew, tmdbCrew{ID: 100, Name: "P", Job: "Executive Producer", Department: "Production"}) // same person and same job de-duplicated
	out := credits(&tmdbCredits{Cast: cast, Crew: crew}, nil, 3)
	kinds := map[pluginv1.PersonKind]int{}
	for _, c := range out {
		kinds[c.GetKind()]++
	}
	if kinds[pluginv1.PersonKind_PERSON_KIND_ACTOR] != 3 || kinds[pluginv1.PersonKind_PERSON_KIND_PRODUCER] != maxProducers {
		t.Errorf("kinds = %v", kinds)
	}
	if n := len(credits(&tmdbCredits{Cast: cast}, nil, 0)); n != 10 {
		t.Errorf("unlimited cast = %d", n)
	}
	// Guest stars de-duplicated, with consecutive order numbers.
	var orders []int32
	for _, c := range credits(nil, []tmdbCast{{ID: 1, Name: "A"}, {ID: 1, Name: "A"}, {ID: 2, Name: "B"}}, 0) {
		orders = append(orders, c.GetOrder())
	}
	if len(orders) != 2 || orders[0] != 0 || orders[1] != 1 {
		t.Errorf("guest orders = %v", orders)
	}
}

func TestOfficialRatingReleaseTypes(t *testing.T) {
	var it tmdbItem
	_ = json.Unmarshal([]byte(`{"release_dates":{"results":[
		{"iso_3166_1":"DE","release_dates":[{"certification":"16","type":1},{"certification":"","type":3},{"certification":"12","type":4}]},
		{"iso_3166_1":"US","release_dates":[{"certification":"PG-13","type":3}]}]}}`), &it)
	for country, want := range map[string]string{"DE": "12", "US": "PG-13", "FR": "PG-13"} {
		if got := officialRating(it, country); got != want {
			t.Errorf("officialRating(%s) = %q, want %q", country, got, want)
		}
	}
}

// TestRevokedToken: when TMDB later returns 401 (the token was revoked), the plugin goes back to needing configuration, and so do later calls.
func TestRevokedToken(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	f.revoked.Store(true)
	ctx := context.Background()
	if _, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("revoked: %v", err)
	}
	n := f.requests.Load()
	if _, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287"}); code(err) != codes.FailedPrecondition || f.requests.Load() != n {
		t.Errorf("after revocation: %v (requests %d -> %d)", err, n, f.requests.Load())
	}
}

// TestUpstreamErrors: an unreachable server or a 5xx returns UNAVAILABLE, and the error message does not contain the token.
func TestUpstreamErrors(t *testing.T) {
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.limiter = 0
		return c
	}
	if _, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{
		keyToken: goodToken, keyBaseURL: "https://127.0.0.1:1/3"}}); err != nil {
		t.Fatal(err)
	}
	_, err := p.GetPerson(context.Background(), &pluginv1.GetPersonRequest{Id: "1"})
	if code(err) != codes.Unavailable || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("unreachable: %v", err)
	}
}

// TestRetryOnServerError: a 5xx is retried once; when it still fails, UNAVAILABLE is returned.
func TestRetryOnServerError(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "502"})
	if err != nil || m.GetId() != "502" || m.GetName() != "Flaky" || f.flaky.Load() != 2 {
		t.Errorf("flaky: %v %v (calls %d)", m, err, f.flaky.Load())
	}
	before := len(f.seen())
	if _, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "503"}); code(err) != codes.Unavailable {
		t.Errorf("503: %v", err)
	}
	if n := len(f.seen()) - before; n != 2 {
		t.Errorf("503 made %d requests, want 2", n)
	}
}

func TestRateLimiter(t *testing.T) {
	f := newFakeTMDB(t)
	c := newClient(f.URL+"/3", goodToken, nil)
	c.http = f.Client()
	start := time.Now()
	for range 25 {
		var out map[string]any
		if err := c.get(context.Background(), "/authentication", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	// The 25th request is sent at the earliest after 24/requestsPerSecond seconds.
	if el := time.Since(start); el < 24*time.Second/requestsPerSecond*9/10 {
		t.Errorf("25 requests took %v; self-limit is %d per second", el, requestsPerSecond)
	}
}

// TestStaleUnauthorizedKeepsNewClient: a 401 for an old client must not clear the new client that Configure just put in place.
func TestStaleUnauthorizedKeepsNewClient(t *testing.T) {
	p := newProvider()
	old, fresh := newClient("https://a/3", goodToken, nil), newClient("https://b/3", goodToken, nil)
	p.client = fresh
	if err := p.upstream(old, errUnauthorized, "item"); code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v", err)
	}
	if p.client != fresh {
		t.Error("a stale 401 cleared the newly configured client")
	}
	_ = p.upstream(fresh, errUnauthorized, "item")
	if p.client != nil {
		t.Error("401 from the current client did not clear it")
	}
}

func TestAPIKeyMode(t *testing.T) {
	f := newFakeTMDB(t)
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.http = f.Client()
		c.limiter = 0
		return c
	}
	ctx := context.Background()
	for _, bad := range []string{"not-a-token", "0123456789abcdef"} {
		resp, _ := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: bad, keyBaseURL: f.URL + "/3"}})
		if resp.GetFieldErrors()[keyToken] == "" {
			t.Errorf("%q accepted", bad)
		}
	}
	resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodKey, keyBaseURL: f.URL + "/3"}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("configure with API key: %v %v", resp, err)
	}
	if _, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "287"}); err != nil {
		t.Errorf("GetPerson with API key: %v", err)
	}
}

// TestAPIKeyRedacted: when the key is in the URL, neither error messages (including net/http's url.Error) nor logs may contain the key.
func TestAPIKeyRedacted(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(b)
	}), nil))
	p := newProvider()
	p.log = logger
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.limiter = 0
		return c
	}
	ctx := context.Background()
	// Unreachable: net/http returns a url.Error whose Error() carries the full URL (including api_key). Configure still accepts and logs one Warn.
	if _, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodKey, keyBaseURL: "https://127.0.0.1:1/3"}}); err != nil {
		t.Fatal(err)
	}
	_, err := p.GetPerson(ctx, &pluginv1.GetPersonRequest{Id: "1"})
	if err == nil || code(err) != codes.Unavailable {
		t.Fatalf("err = %v", err)
	}
	// Get the error directly from the client, to confirm that the path of url.Error is masked too.
	c := newClient("https://127.0.0.1:1/3", goodKey, logger)
	c.limiter = 0
	var out map[string]any
	rawErr := c.get(ctx, "/movie/1", nil, &out)
	mu.Lock()
	logged := logs.String()
	mu.Unlock()
	for name, text := range map[string]string{"provider error": err.Error(), "client error": rawErr.Error(), "log": logged} {
		if strings.Contains(text, goodKey) {
			t.Errorf("%s leaks the API key: %s", name, text)
		}
	}
	if !strings.Contains(logged, "could not verify") {
		t.Errorf("expected a warning in the log:\n%s", logged)
	}
	// The masking function itself: replaces the key when a url.Error message carries the full URL.
	ue := &url.Error{Op: "Get", URL: "https://api.themoviedb.org/3/movie/1?api_key=" + goodKey, Err: errors.New("boom")}
	if got := c.redact(ue).Error(); strings.Contains(got, goodKey) || !strings.Contains(got, "[REDACTED]") {
		t.Errorf("redact(url.Error) = %q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// TestTrailers: only YouTube Trailers and Teasers are taken; language first (target language plus region -> fallback chain -> no language tag),
// within the same language a Trailer comes before a Teaser and official ones come first, and duplicates keep only one; the request carries include_video_language.
func TestTrailers(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550",
		Languages: zhTW("zh-CN")})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, tr := range m.GetTrailers() {
		keys = append(keys, strings.TrimPrefix(tr.GetUrl(), "https://www.youtube.com/watch?v="))
	}
	// zh-TW: Trailer, Teaser; zh-CN; en; no language. Vimeo and Clip are not taken.
	if got := strings.Join(keys, ","); got != "tw-trailer,tw-teaser,cn1,en1,none" {
		t.Errorf("trailers = %s", got)
	}
	first := m.GetTrailers()[0]
	if first.GetLanguage() != "zh-TW" || first.GetKind() != "trailer" || first.GetOfficial() {
		t.Errorf("first trailer = %v", first)
	}
	var sawLang bool
	for _, r := range f.seen() {
		if strings.Contains(r, "/movie/550") && strings.Contains(r, "videos") {
			sawLang = true
		}
	}
	if !sawLang {
		t.Errorf("movie request did not ask for videos: %v", f.seen())
	}
	if got := videoLanguages(chain(zhTW("ja"))); got != "zh,ja,en,null" {
		t.Errorf("videoLanguages = %s", got)
	}
	if len(trailers(nil, chain(nil))) != 0 || len(trailers(&tmdbVideos{}, chain(nil))) != 0 {
		t.Error("no videos should give no trailers")
	}
}

// TestSeriesStatus: TMDB's status mapped to the protocol (change order 0019).
func TestSeriesStatus(t *testing.T) {
	for in, want := range map[string]pluginv1.SeriesStatus{
		"Returning Series": pluginv1.SeriesStatus_SERIES_STATUS_CONTINUING, "In Production": pluginv1.SeriesStatus_SERIES_STATUS_CONTINUING,
		"Planned": pluginv1.SeriesStatus_SERIES_STATUS_CONTINUING, "Pilot": pluginv1.SeriesStatus_SERIES_STATUS_CONTINUING,
		"Ended": pluginv1.SeriesStatus_SERIES_STATUS_ENDED, "Canceled": pluginv1.SeriesStatus_SERIES_STATUS_ENDED,
		"": pluginv1.SeriesStatus_SERIES_STATUS_UNSPECIFIED, "Rumored": pluginv1.SeriesStatus_SERIES_STATUS_UNSPECIFIED,
	} {
		if got := seriesStatus(pluginv1.ItemType_ITEM_TYPE_SERIES, in); got != want {
			t.Errorf("seriesStatus(%q) = %v, want %v", in, got, want)
		}
	}
	if got := seriesStatus(pluginv1.ItemType_ITEM_TYPE_MOVIE, "Ended"); got != pluginv1.SeriesStatus_SERIES_STATUS_UNSPECIFIED {
		t.Errorf("movie status = %v", got)
	}
	it := tmdbItem{ID: 1, Name: "x", Status: "Ended"}
	if m := newProvider().titleMetadata(pluginv1.ItemType_ITEM_TYPE_SERIES, it, chain(nil)); m.GetStatus() != pluginv1.SeriesStatus_SERIES_STATUS_ENDED {
		t.Errorf("titleMetadata status = %v", m.GetStatus())
	}
}

// TestResultLanguage: ItemMetadata.language is the language the result actually uses (change order 0018).
func TestResultLanguage(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	for _, tc := range []struct{ id, lang, want string }{
		{"1678", "zh-TW", "zh-TW"}, // Taiwan alias
		{"1678", "zh-CN", "zh-CN"},
		{"1678", "zh-HK", "zh-HK"}, // the CN translation converted to HK script
		{"1678", "en-US", "en-US"},
		{"1678", "ja", "ja"},    // the original text is Japanese
		{"1678", "fr", "en-US"}, // falls back to English in the fallback chain
		{"2", "en", "en"},       // falls back to the original title: the original language (this one has no original_language)
	} {
		m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: tc.id,
			Languages: &pluginv1.Languages{Language: tc.lang}})
		if err != nil {
			t.Fatal(err)
		}
		want := tc.want
		if tc.id == "2" {
			want = "" // no original_language: not reported
		}
		if m.GetLanguage() != want {
			t.Errorf("%s %s: language %q, want %q", tc.id, tc.lang, m.GetLanguage(), want)
		}
	}
	// Simplified/Traditional conversion off: what is obtained in Simplified stays Simplified.
	p.conv.setOff(true)
	if m, _ := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "2",
		Languages: zhTW()}); m.GetLanguage() != "zh-TW" {
		t.Errorf("conversion off, zh-TW translation: %q", m.GetLanguage())
	}
	p.conv.setOff(false)
	s, e := int32(1), int32(1)
	season, _ := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_SEASON, SeriesId: "1399", SeasonNumber: &s, Languages: zhTW()})
	ep, _ := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399", SeasonNumber: &s, EpisodeNumber: &e, Languages: zhTW()})
	if season.GetLanguage() != "zh-TW" || ep.GetLanguage() != "zh-TW" { // the episode name comes from zh-CN converted to Traditional
		t.Errorf("season %q episode %q", season.GetLanguage(), ep.GetLanguage())
	}
	jp, _ := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesId: "1399", SeasonNumber: &s, EpisodeNumber: &e,
		Languages: &pluginv1.Languages{Language: "ja"}})
	if jp.GetLanguage() != "en-US" { // Japanese is a placeholder, so it falls back to English
		t.Errorf("ja episode language %q", jp.GetLanguage())
	}
}

// TestCollectionEmptyTranslationFallsBack: when the translation is empty it falls back to TMDB's original name, so filling in the collection name (change order 0078) never gets an empty string.
func TestCollectionEmptyTranslationFallsBack(t *testing.T) {
	p := configured(t, newFakeTMDB(t))
	for _, lang := range []*pluginv1.Languages{zhTW(), {Language: "zh-CN"}, {Language: "en"}} {
		col, err := p.GetCollection(context.Background(), &pluginv1.GetCollectionRequest{Id: "11", Languages: lang})
		if err != nil {
			t.Fatal(err)
		}
		if col.GetName() != "Plain Saga" || col.GetOverview() != "plain overview" {
			t.Errorf("lang %v: name %q overview %q, want the untranslated TMDB text", lang.GetLanguage(), col.GetName(), col.GetOverview())
		}
	}
}

// TestTrimCollectionSuffix: trim_collection_suffix is off by default; when on, it removes the trailing Chinese "series" marker (in parentheses) and the "Collection" suffix;
// an empty result, or such wording in the middle of a name, is left alone.
func TestTrimCollectionSuffix(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	get := func() string {
		col, err := p.GetCollection(ctx, &pluginv1.GetCollectionRequest{Id: "10", Languages: zhTW()})
		if err != nil {
			t.Fatal(err)
		}
		return col.GetName()
	}
	if got := get(); got != "星球大戰（系列）" {
		t.Errorf("default name = %q, want the suffix kept", got)
	}
	v := map[string]string{keyToken: goodToken, keyBaseURL: f.URL + "/3", keyTrimSeries: "true"}
	if resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: v}); err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("configure: %v %v", resp, err)
	}
	if got := get(); got != "星球大戰" {
		t.Errorf("trimmed name = %q", got)
	}
	for in, want := range map[string]string{
		"Star Wars Collection": "Star Wars", "Star Wars collection": "Star Wars", "星際大戰(系列)": "星際大戰", "星際大戰 （系列）": "星際大戰",
		"Collection": "Collection", "（系列）": "（系列）", "The Collection Agency": "The Collection Agency", "魔戒系列": "魔戒系列", "": "",
	} {
		if got := p.collectionName(in); got != want {
			t.Errorf("collectionName(%q) = %q, want %q", in, got, want)
		}
	}
	// Set back to false: restored immediately.
	v[keyTrimSeries] = "false"
	_, _ = p.Configure(ctx, &pluginv1.ConfigureRequest{Values: v})
	if got := get(); got != "星球大戰（系列）" {
		t.Errorf("after turning it off = %q", got)
	}
}

// TestLogos: logos (the logos of TMDB images) are ordered by the poster's language rules: target language and region -> same script -> fallback chain -> no language;
// SVG is skipped and only PNG is kept. When there is no logo no image is filled in.
func TestLogos(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	logos := func(lang string) []string {
		m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "550", Languages: &pluginv1.Languages{Language: lang}})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, i := range m.GetImages() {
			if i.GetType() == pluginv1.ImageType_IMAGE_TYPE_LOGO {
				if !strings.HasPrefix(i.GetUrl(), "https://") || !strings.HasSuffix(i.GetUrl(), ".png") {
					t.Errorf("logo url %q", i.GetUrl())
				}
				out = append(out, path.Base(i.GetUrl())+"/"+i.GetLanguage())
			}
		}
		return out
	}
	if got := strings.Join(logos("zh-TW"), " "); got != "logo-tw.png/zh-TW logo-cn.png/zh-CN logo-en.png/en-US logo-none.png/" {
		t.Errorf("zh-TW logos = %s", got)
	}
	if got := strings.Join(logos("en"), " "); got != "logo-en.png/en-US logo-none.png/ logo-cn.png/zh-CN logo-tw.png/zh-TW" {
		t.Errorf("en logos = %s", got)
	}
	// An item without a logo: no Logo image is produced.
	if got := convertImages(pluginv1.ImageType_IMAGE_TYPE_LOGO, nil, "", chain(zhTW())); len(got) != 0 {
		t.Errorf("no logos = %v", got)
	}
}

// TestCreditGender (change order 0079): cast and crew of movie credits and series aggregate_credits both carry gender;
// 0 or an unrecognized number is unknown.
func TestCreditGender(t *testing.T) {
	var c tmdbCredits
	raw := `{"cast":[{"id":1,"name":"F","gender":1,"order":0},{"id":2,"name":"M","gender":2,"order":1},{"id":3,"name":"N","gender":3,"order":2},
	  {"id":4,"name":"U","gender":0,"order":3},{"id":5,"name":"X","gender":9,"order":4},{"id":6,"name":"Missing","order":5}],
	  "crew":[{"id":7,"name":"D","job":"Director","department":"Directing","gender":2},
	  {"id":8,"name":"W","department":"Writing","gender":1,"jobs":[{"job":"Screenplay"}]}],
	  "guest_stars":[{"id":9,"name":"G","gender":1}]}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	got := map[string]pluginv1.Gender{}
	for _, cr := range credits(&c, nil, 0) {
		got[cr.GetName()+"/"+cr.GetKind().String()] = cr.GetGender()
	}
	for k, want := range map[string]pluginv1.Gender{
		"F/PERSON_KIND_ACTOR": pluginv1.Gender_GENDER_FEMALE, "M/PERSON_KIND_ACTOR": pluginv1.Gender_GENDER_MALE,
		"N/PERSON_KIND_ACTOR": pluginv1.Gender_GENDER_OTHER, "U/PERSON_KIND_ACTOR": pluginv1.Gender_GENDER_UNSPECIFIED,
		"X/PERSON_KIND_ACTOR": pluginv1.Gender_GENDER_UNSPECIFIED, "Missing/PERSON_KIND_ACTOR": pluginv1.Gender_GENDER_UNSPECIFIED,
		"D/PERSON_KIND_DIRECTOR": pluginv1.Gender_GENDER_MALE, "W/PERSON_KIND_WRITER": pluginv1.Gender_GENDER_FEMALE,
		"G/PERSON_KIND_GUEST_STAR": pluginv1.Gender_GENDER_FEMALE,
	} {
		if g, ok := got[k]; !ok || g != want {
			t.Errorf("%s: gender %v (present %v), want %v; all: %v", k, g, ok, want, got)
		}
	}
}

// TestSearchPerson (change order 0079): confirmed only when a work matches; each search has an upper limit on requests.
func TestSearchPerson(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	movie := func(ids map[string]string) []*pluginv1.PersonWork {
		return []*pluginv1.PersonWork{{Name: "Fight Club", ItemType: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: ids}}
	}
	run := func(req *pluginv1.SearchPersonRequest) (map[string]bool, int64) {
		t.Helper()
		before := f.requests.Load()
		resp, err := p.SearchPerson(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, c := range resp.GetCandidates() {
			got[c.GetPerson().GetId()] = c.GetConfirmed()
			if c.GetPerson().GetProviderIds()["tmdb"] != c.GetPerson().GetId() || c.GetPerson().GetName() == "" {
				t.Errorf("candidate %v lacks id or name", c.GetPerson())
			}
		}
		return got, f.requests.Load() - before
	}
	t.Run("known_for confirms; the same-name person is not confirmed", func(t *testing.T) {
		got, n := run(&pluginv1.SearchPersonRequest{Name: "Alice Example", Works: movie(map[string]string{"tmdb": "550"})})
		if len(got) != 2 || !got["501"] || got["502"] || n > personSearchBudget {
			t.Errorf("candidates %v, requests %d", got, n)
		}
	})
	t.Run("combined_credits confirms", func(t *testing.T) {
		got, n := run(&pluginv1.SearchPersonRequest{Name: "Bob Example", Works: movie(map[string]string{"tmdb": "550"})})
		if len(got) != 2 || !got["601"] || got["602"] || n != 3 {
			t.Errorf("candidates %v, requests %d, want 601 confirmed in 3 requests (search + 2 credits)", got, n)
		}
	})
	t.Run("a series with the same numeric id is a different work", func(t *testing.T) {
		got, _ := run(&pluginv1.SearchPersonRequest{Name: "Bob Example", Works: []*pluginv1.PersonWork{
			{ItemType: pluginv1.ItemType_ITEM_TYPE_SERIES, ProviderIds: map[string]string{"tmdb": "550"}}}})
		if got["601"] || !got["602"] {
			t.Errorf("candidates %v, want only 602 (tv 550)", got)
		}
	})
	t.Run("imdb hint is resolved with /find", func(t *testing.T) {
		got, n := run(&pluginv1.SearchPersonRequest{Name: "Alice Example", Works: movie(map[string]string{"imdb": "tt0137523"})})
		if !got["501"] || got["502"] || n != 3 {
			t.Errorf("candidates %v, requests %d, want 501 confirmed in 3 requests (search + find + credits)", got, n)
		}
	})
	t.Run("no usable work hint means nothing is confirmed and no extra requests", func(t *testing.T) {
		got, n := run(&pluginv1.SearchPersonRequest{Name: "Alice Example", Works: movie(map[string]string{"other": "x"})})
		if len(got) != 2 || got["501"] || got["502"] || n != 1 {
			t.Errorf("candidates %v, requests %d", got, n)
		}
		if got, n = run(&pluginv1.SearchPersonRequest{Name: "Alice Example"}); got["501"] || n != 1 {
			t.Errorf("no works: %v, requests %d", got, n)
		}
	})
	t.Run("alias is searched when the name finds nothing", func(t *testing.T) {
		got, n := run(&pluginv1.SearchPersonRequest{Name: "Nobody Here", Aliases: []string{"Alice Example"}, Works: movie(map[string]string{"tmdb": "550"})})
		if !got["501"] || got["502"] || n != 3 {
			t.Errorf("candidates %v, requests %d, want 501 confirmed in 3 requests (2 searches + credits of 502)", got, n)
		}
	})
	t.Run("no match is an empty list", func(t *testing.T) {
		if got, _ := run(&pluginv1.SearchPersonRequest{Name: "Nobody Here"}); len(got) != 0 {
			t.Errorf("candidates %v", got)
		}
	})
	t.Run("empty name is invalid", func(t *testing.T) {
		if _, err := p.SearchPerson(ctx, &pluginv1.SearchPersonRequest{}); code(err) != codes.InvalidArgument {
			t.Errorf("err = %v", err)
		}
	})
	m, err := p.Describe(ctx, &pluginv1.DescribeRequest{})
	if err != nil || !slices.Contains(m.GetCapabilities(), pluginv1.CapabilityMetadataPersonSearch) {
		t.Errorf("Describe capabilities = %v, %v", m.GetCapabilities(), err)
	}
}

// After a 429 the rate is halved for 60 seconds (it does not rush back to the limit and get throttled again when the pause ends); it recovers after that.
func TestSlowdownAfterRateLimit(t *testing.T) {
	c := newClient("http://example.invalid", goodToken, nil)
	measure := func() time.Duration {
		c.mu.Lock()
		c.next = time.Time{}
		c.mu.Unlock()
		_ = c.wait(context.Background())
		c.mu.Lock()
		defer c.mu.Unlock()
		return time.Until(c.next)
	}
	normal := measure()
	if normal > c.limiter {
		t.Fatalf("normal step %v exceeds the limiter %v", normal, c.limiter)
	}
	c.mu.Lock()
	c.slowUntil = time.Now().Add(time.Minute)
	c.mu.Unlock()
	slow := measure()
	if slow < c.limiter+c.limiter/2 {
		t.Errorf("slowed step %v, want about twice the limiter %v", slow, c.limiter)
	}
	c.mu.Lock()
	c.slowUntil = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if again := measure(); again > c.limiter {
		t.Errorf("after the slowdown the step %v must be back to the limiter %v", again, c.limiter)
	}
}

// Throughput (with request latency): the old setting (20 per second, 4 concurrent) and the new setting (40 per second, 12 concurrent) on a server with 150 ms per request,
// using 24 concurrent callers each sending 8 requests. The new setting's throughput must be clearly higher and must stay within the self-limit's upper bound. The results are logged (-v).
func TestThroughputWithLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 15 seconds")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	run := func(rps, conc int) float64 {
		c := newClient(srv.URL, goodToken, nil)
		c.limiter = time.Second / time.Duration(rps)
		c.sem = make(chan struct{}, conc)
		const callers, each = 24, 8
		var wg sync.WaitGroup
		start := time.Now()
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range each {
					var out map[string]any
					_ = c.get(context.Background(), "/x", nil, &out)
				}
			}()
		}
		wg.Wait()
		return float64(callers*each) / time.Since(start).Seconds()
	}
	before := run(20, 4)
	after := run(requestsPerSecond, maxConcurrent)
	t.Logf("throughput with 150 ms latency: before (20/s, 4 concurrent) = %.1f req/s; after (%d/s, %d concurrent) = %.1f req/s", before, requestsPerSecond, maxConcurrent, after)
	if after < before*1.5 {
		t.Errorf("after %.1f req/s is not clearly above before %.1f req/s", after, before)
	}
	if after > float64(requestsPerSecond)*1.1 {
		t.Errorf("after %.1f req/s exceeds the self-limit %d/s", after, requestsPerSecond)
	}
}

// TestOriginalLanguageIsReported: a work's original language (change order 0120) comes from TMDB's original_language; empty when not given, "xx" is unknown and "cn" is zh.
func TestOriginalLanguageIsReported(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "1678", Languages: zhTW()})
	if err != nil {
		t.Fatal(err)
	}
	if m.GetOriginalLanguage() != "ja" {
		t.Errorf("OriginalLanguage = %q, want ja", m.GetOriginalLanguage())
	}
	if m, err = p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "2", Languages: zhTW()}); err != nil || m.GetOriginalLanguage() != "" {
		t.Errorf("a title without original_language: %q %v", m.GetOriginalLanguage(), err)
	}
	for in, want := range map[string]string{"JA": "ja", " en ": "en", "xx": "", "cn": "zh", "": ""} {
		if got := originalLanguage(in); got != want {
			t.Errorf("originalLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOriginalLanguageImagesAreRequested: when the original language is not on the fallback chain, one extra /images request for only the original language is sent and added to the candidates (ordering is still decided by the host's
// image language preference); when the original language is already on the fallback chain, or there is none, no extra request is sent.
func TestOriginalLanguageImagesAreRequested(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	get := func(lang string) *pluginv1.ItemMetadata {
		t.Helper()
		m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "1678", Languages: &pluginv1.Languages{Language: lang}})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	count := func() int {
		n := 0
		for _, s := range f.seen() {
			if strings.Contains(s, "/3/movie/1678/images") {
				n++
			}
		}
		return n
	}
	m := get("zh-CN")
	var posters, backdrops []string
	for _, im := range m.GetImages() {
		switch im.GetType() {
		case pluginv1.ImageType_IMAGE_TYPE_PRIMARY:
			posters = append(posters, im.GetLanguage())
		case pluginv1.ImageType_IMAGE_TYPE_BACKDROP:
			backdrops = append(backdrops, im.GetLanguage())
		}
	}
	if len(posters) != 2 || posters[0] != "ja-JP" || len(backdrops) != 1 || backdrops[0] != "ja" || count() != 1 {
		t.Fatalf("posters %v backdrops %v extra requests %d; want the two ja posters and the ja backdrop from one extra request", posters, backdrops, count())
	}
	// The original language is on the fallback chain (Japanese): no extra request.
	get("ja")
	if count() != 1 {
		t.Errorf("ja is already in the chain but an extra images request was made (%d)", count())
	}
	// A work whose original language is English (en is on the fallback chain of zh-CN), and a work with no original language: no extra request.
	if m, err := p.GetItem(ctx, &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "2", Languages: zhTW()}); err != nil || len(m.GetImages()) != 0 {
		t.Errorf("a title without an original language: %v %v", m.GetImages(), err)
	}
}

// Stills: the backdrop count limit (setting max_backdrops, default 20, 0 means all); the ones kept are the first in the ordering (language preference chain, rating, resolution), without duplicates;
// the plugin downloads no image (the fake server of this test has only API paths), and there are no per-image requests. Other kinds of images are not affected.
func TestBackdropLimit(t *testing.T) {
	f := newFakeTMDB(t)
	get := func(values map[string]string) []string {
		t.Helper()
		p := newProvider()
		p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
			c := newClient(base, token, log)
			c.http = f.Client()
			c.limiter = 0
			c.retry = time.Millisecond
			return c
		}
		v := map[string]string{keyToken: goodToken, keyBaseURL: f.URL + "/3"}
		for k, x := range values {
			v[k] = x
		}
		if resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: v}); err != nil || len(resp.GetFieldErrors()) > 0 {
			t.Fatalf("Configure(%v) = %v, %v", values, resp, err)
		}
		before := len(f.seen())
		m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "3030", Languages: &pluginv1.Languages{Language: "en"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range f.seen()[before:] {
			if !strings.HasPrefix(path, "/3/") {
				t.Errorf("request outside the API: %s", path)
			}
		}
		if n := len(f.seen()) - before; n > 2 {
			t.Errorf("%d requests for one item, want at most the detail and one image-language request", n)
		}
		var urls []string
		seen := map[string]bool{}
		posters := 0
		for _, im := range m.GetImages() {
			if im.GetType() == pluginv1.ImageType_IMAGE_TYPE_PRIMARY {
				posters++
			}
			if im.GetType() != pluginv1.ImageType_IMAGE_TYPE_BACKDROP {
				continue
			}
			if !strings.HasPrefix(im.GetUrl(), "https://") || seen[im.GetUrl()] {
				t.Errorf("backdrop url %q (duplicate or not https)", im.GetUrl())
			}
			seen[im.GetUrl()] = true
			urls = append(urls, im.GetUrl())
		}
		if posters != 1 {
			t.Errorf("%d posters, want 1 (the limit applies to backdrops only)", posters)
		}
		return urls
	}
	all := get(map[string]string{keyMaxBackdrops: "0"})
	if len(all) != 30 {
		t.Fatalf("max_backdrops=0 returned %d backdrops, want all 30", len(all))
	}
	def := get(nil)
	if len(def) != defaultMaxBackdrops || defaultMaxBackdrops != 20 {
		t.Fatalf("the default returned %d backdrops, want %d", len(def), defaultMaxBackdrops)
	}
	if few := get(map[string]string{keyMaxBackdrops: "5"}); len(few) != 5 {
		t.Errorf("max_backdrops=5 returned %d", len(few))
	}
	for i := range def { // truncation takes the first in the ordering, the order is unchanged
		if def[i] != all[i] {
			t.Fatalf("backdrop %d of the capped list = %s, want %s (the best ones, same order)", i, def[i], all[i])
		}
	}
	if big := get(map[string]string{keyMaxBackdrops: "200"}); len(big) != 30 {
		t.Errorf("a limit above the count returned %d", len(big))
	}
}

func TestBackdropLimitValidationAndDescribe(t *testing.T) {
	p := newProvider()
	for _, bad := range []string{"-1", "201", "abc", "1.5"} {
		resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, keyMaxBackdrops: bad}, Language: "zh-TW"})
		if err != nil || resp.GetFieldErrors()[keyMaxBackdrops] == "" {
			t.Errorf("max_backdrops=%q accepted: %v %v", bad, resp, err)
		}
	}
	for _, lang := range []string{"en", "zh-TW", "zh-CN"} {
		m, err := p.Describe(context.Background(), &pluginv1.DescribeRequest{Language: lang})
		if err != nil {
			t.Fatal(err)
		}
		var f *pluginv1.ConfigField
		for _, x := range m.GetConfigFields() {
			if x.GetKey() == keyMaxBackdrops {
				f = x
			}
		}
		if f == nil || f.GetType() != pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_INT || f.GetDefaultValue() != "20" || f.GetMin() != 0 || f.GetMax() != 200 || f.GetLabel() == "" || f.GetDescription() == "" {
			t.Errorf("%s: field = %v", lang, f)
		}
	}
}

// images.person (change request 0156): all the profile photos TMDB has, best first, found by the tmdb provider ID only.
func TestPersonImages(t *testing.T) {
	f := newFakeTMDB(t)
	p := configured(t, f)
	ctx := context.Background()
	resp, err := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "287", "imdb": "nm0000093"}, Name: "Brad Pitt"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range resp.GetImages() {
		if i.GetType() != pluginv1.ImageType_IMAGE_TYPE_PRIMARY || i.GetLanguage() != "" {
			t.Errorf("image = %v; want a primary photo without text", i)
		}
		got = append(got, i.GetUrl())
	}
	if want := "/best.jpg /mid.jpg /low.jpg"; strings.Join(trimBase(got), " ") != want {
		t.Errorf("order = %v; want %s (rating, svg skipped)", got, want)
	}
	if lim, _ := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "287"}, LimitPerType: 2}); len(lim.GetImages()) != 2 {
		t.Errorf("limit 2 = %d images", len(lim.GetImages()))
	}
	// No tmdb ID (only a name): nothing is guessed. Unknown person: an empty list, not an error.
	if none, err := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, Name: "Brad Pitt", ProviderIds: map[string]string{"imdb": "nm0000093"}}); err != nil || len(none.GetImages()) != 0 {
		t.Errorf("name only = %v, %v", none, err)
	}
	if none, err := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "99999999"}}); err != nil || len(none.GetImages()) != 0 {
		t.Errorf("unknown person = %v, %v", none, err)
	}
	// This plugin does not serve movie or series images through this call.
	if _, err := p.GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "550"}}); code(err) != codes.InvalidArgument {
		t.Errorf("movie images = %v, want InvalidArgument", err)
	}
	// Without a token it is awaiting configuration like the other calls.
	if _, err := newProvider().GetImages(ctx, &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "287"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("unconfigured = %v, want FailedPrecondition", err)
	}
}

// trimBase drops the image base URL so the test reads the file paths.
func trimBase(urls []string) []string {
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = u[strings.LastIndex(u, "/"):]
	}
	return out
}
