package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginapi "github.com/kryspal/plugin-sdk"
	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// Tests for the notices that tell the host what happened at TMDB. All titles are synthetic.

// stubProvider is a provider configured against a server whose answer for every path but /authentication is given by h.
func stubProvider(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *provider {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/authentication") {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	p := newProvider()
	p.newClient = func(base, token string, log *slog.Logger) *tmdbClient {
		c := newClient(base, token, log)
		c.http = srv.Client()
		c.limiter = 0
		c.retry = time.Millisecond
		return c
	}
	resp, err := p.Configure(context.Background(), &pluginv1.ConfigureRequest{Values: map[string]string{keyToken: goodToken, keyBaseURL: srv.URL + "/3"}})
	if err != nil || len(resp.GetFieldErrors()) > 0 {
		t.Fatalf("Configure = %v, %v", resp, err)
	}
	return p
}

// withNotices makes the host announce that it keeps notices.
func withNotices(t *testing.T) {
	t.Setenv(pluginapi.EnvHostFeatures, pluginapi.HostFeatureScrapeNotices)
}

// A title whose original language is not on the language chain needs one more request for its images. When that request fails the
// main result still comes back, now marked incomplete with a notice that says why, so the host asks again later.
func TestTitleImagesGapIsReported(t *testing.T) {
	for _, c := range []struct {
		name      string
		images    func(w http.ResponseWriter)
		status    pluginv1.NoticeStatus
		wantRetry uint32
	}{
		{"rate limited", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}, pluginv1.NoticeStatus_NOTICE_STATUS_BLOCKED, 120},
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, pluginv1.NoticeStatus_NOTICE_STATUS_ERROR, 0},
		{"not found", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, pluginv1.NoticeStatus_NOTICE_STATUS_NOT_FOUND, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/images") {
					c.images(w)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 700, "title": "Synthetic Title", "original_title": "Synthetic Title", "original_language": "ja"})
			})
			req := &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "700", Languages: &pluginv1.Languages{Language: "en"}}

			// A host without the feature gets the plain result.
			m, err := p.GetItem(context.Background(), req)
			if err != nil || m.GetName() == "" || len(m.GetNotices()) != 0 || m.GetIncomplete() || m.GetRetryAfterSeconds() != 0 {
				t.Fatalf("old host: %v, %v; want the plain result", m, err)
			}

			withNotices(t)
			m, err = p.GetItem(context.Background(), req)
			if err != nil || m.GetName() == "" {
				t.Fatalf("GetItem = %v, %v", m, err)
			}
			if len(m.GetNotices()) != 1 || m.GetNotices()[0].GetStatus() != c.status || m.GetNotices()[0].GetSource() != "TMDB" || m.GetNotices()[0].GetMessage() == "" {
				t.Errorf("notices = %v, want one %v notice from TMDB", m.GetNotices(), c.status)
			}
			if !m.GetIncomplete() || m.GetRetryAfterSeconds() != c.wantRetry {
				t.Errorf("incomplete = %v, retry = %d; want true, %d", m.GetIncomplete(), m.GetRetryAfterSeconds(), c.wantRetry)
			}
		})
	}
}

// A complete result carries no notices and is not marked incomplete.
func TestCompleteTitleHasNoNotices(t *testing.T) {
	withNotices(t)
	p := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 700, "title": "Synthetic Title", "original_title": "Synthetic Title"})
	})
	m, err := p.GetItem(context.Background(), &pluginv1.GetItemRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Id: "700", Languages: &pluginv1.Languages{Language: "en"}})
	if err != nil || len(m.GetNotices()) != 0 || m.GetIncomplete() {
		t.Errorf("GetItem = %v, %v; want no notices", m, err)
	}
}

// Notices never carry the credential, a URL or a response body, whatever the failure.
func TestNoticesCarryNoSecrets(t *testing.T) {
	for _, err := range []error{errUnauthorized, errNotFound, context.DeadlineExceeded, newClient("https://x.invalid/3", goodKey, nil).redact(errBoom())} {
		n, _ := notice(err)
		if strings.Contains(n.GetMessage(), goodKey) || strings.Contains(n.GetMessage(), goodToken) || strings.Contains(n.GetMessage(), "http") {
			t.Errorf("notice %q for %v leaks something", n.GetMessage(), err)
		}
		if len(n.GetMessage()) > 200 || len(n.GetSource()) > 48 {
			t.Errorf("notice %v exceeds the protocol limits", n)
		}
	}
}

func errBoom() error { return &boomError{} }

type boomError struct{}

func (*boomError) Error() string {
	return "get https://x.invalid/3/movie/1?api_key=" + goodKey + ": boom"
}

func TestNoticeStatuses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want pluginv1.NoticeStatus
	}{
		{errUnauthorized, pluginv1.NoticeStatus_NOTICE_STATUS_NEEDS_LOGIN},
		{errNotFound, pluginv1.NoticeStatus_NOTICE_STATUS_NOT_FOUND},
		{context.DeadlineExceeded, pluginv1.NoticeStatus_NOTICE_STATUS_TIMEOUT},
		{errBoom(), pluginv1.NoticeStatus_NOTICE_STATUS_ERROR},
	} {
		if n, _ := notice(c.err); n.GetStatus() != c.want {
			t.Errorf("notice(%v) = %v, want %v", c.err, n.GetStatus(), c.want)
		}
	}
}

// A search that finds nothing says so; one that finds something does not.
func TestSearchNotFoundNotice(t *testing.T) {
	p := stubProvider(t, func(w http.ResponseWriter, r *http.Request) {
		results := []any{}
		if r.URL.Query().Get("query") == "Known Title" {
			results = append(results, map[string]any{"id": 1, "title": "Known Title", "original_title": "Known Title", "release_date": "2000-01-01"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	search := func(name string) *pluginv1.SearchResponse {
		t.Helper()
		resp, err := p.Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := search("Nothing Like It"); len(resp.GetNotices()) != 0 {
		t.Errorf("old host: notices = %v, want none", resp.GetNotices())
	}
	withNotices(t)
	resp := search("Nothing Like It")
	if len(resp.GetCandidates()) != 0 || len(resp.GetNotices()) != 1 || resp.GetNotices()[0].GetStatus() != pluginv1.NoticeStatus_NOTICE_STATUS_NOT_FOUND {
		t.Errorf("empty search: %v, want one not_found notice", resp)
	}
	if resp := search("Known Title"); len(resp.GetCandidates()) == 0 || len(resp.GetNotices()) != 0 {
		t.Errorf("successful search: %v, want candidates and no notice", resp)
	}
	if resp, err := p.Search(context.Background(), &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE}); err != nil || len(resp.GetNotices()) != 0 {
		t.Errorf("a request without a name or ID asked nothing: %v, %v", resp, err)
	}
}

// A hinted search that skips a failed later query tells the user the answer may be thinner than usual. The failure is not an error.
func TestHintedSearchGapIsReported(t *testing.T) {
	f := newHintTMDB(t)
	f.results["movie|First"] = []map[string]any{movieHit(1, "Weak Match", "Weak Match", "2010-01-01", nil)}
	f.fail["Second"] = http.StatusInternalServerError
	p := f.provider(t)
	req := &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "First", Titles: []*pluginv1.TitleHint{
		hint("First", rolePrimary, 1), hint("Second", roleAlt, 0.5)}}

	resp, err := p.Search(context.Background(), req)
	if err != nil || len(resp.GetCandidates()) != 1 || len(resp.GetNotices()) != 0 {
		t.Fatalf("old host: %v, %v", resp, err)
	}
	withNotices(t)
	resp, err = p.Search(context.Background(), req)
	if err != nil || len(resp.GetCandidates()) != 1 {
		t.Fatalf("Search = %v, %v", resp, err)
	}
	if len(resp.GetNotices()) != 1 || resp.GetNotices()[0].GetStatus() != pluginv1.NoticeStatus_NOTICE_STATUS_ERROR {
		t.Errorf("notices = %v, want one error notice", resp.GetNotices())
	}
}

// A person without photos at TMDB is answered with an empty list and a not_found notice.
func TestPersonImagesNotFoundNotice(t *testing.T) {
	p := stubProvider(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	req := &pluginv1.GetImagesRequest{Type: pluginv1.ItemType_ITEM_TYPE_PERSON, ProviderIds: map[string]string{"tmdb": "287"}}
	resp, err := p.GetImages(context.Background(), req)
	if err != nil || len(resp.GetImages()) != 0 || len(resp.GetNotices()) != 0 {
		t.Fatalf("old host: %v, %v", resp, err)
	}
	withNotices(t)
	resp, err = p.GetImages(context.Background(), req)
	if err != nil || len(resp.GetNotices()) != 1 || resp.GetNotices()[0].GetStatus() != pluginv1.NoticeStatus_NOTICE_STATUS_NOT_FOUND {
		t.Errorf("GetImages = %v, %v; want one not_found notice", resp, err)
	}
}
