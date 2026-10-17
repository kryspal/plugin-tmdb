package main

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"

	pluginapi "github.com/kryspal/plugin-sdk"
	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

// noticeSource is the source name the user sees next to a notice.
const noticeSource = "TMDB"

// Notices say what happened at TMDB when a request gave less than usual. The messages are fixed sentences: they never carry the token, a URL or a response body.
// A host older than SDK v0.2.0 ignores notices and treats an incomplete result as final, so nothing is sent to it (host feature scrape_notices).

// hostShowsNotices reports whether the host keeps notices and asks again for incomplete results.
func hostShowsNotices() bool {
	return pluginapi.HostFeatures(os.Getenv)[pluginapi.HostFeatureScrapeNotices]
}

// notice describes a failed or empty request to TMDB. retry is the number of seconds after which the host should ask again (0: the host's default).
func notice(err error) (n *pluginv1.Notice, retry uint32) {
	n = &pluginv1.Notice{Source: noticeSource}
	var ne net.Error
	switch {
	case errors.Is(err, errUnauthorized):
		n.Status, n.Message = pluginv1.NoticeStatus_NOTICE_STATUS_NEEDS_LOGIN, "TMDB rejected the API token. Check the token on the plugin settings page."
	case errors.Is(err, errNotFound):
		n.Status, n.Message = pluginv1.NoticeStatus_NOTICE_STATUS_NOT_FOUND, "TMDB has no entry for this."
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		n.Status, n.Message = pluginv1.NoticeStatus_NOTICE_STATUS_TIMEOUT, "TMDB did not answer in time."
	default:
		if rl, ok := errors.AsType[*sdk.RateLimitError](err); ok {
			n.Status, n.Message = pluginv1.NoticeStatus_NOTICE_STATUS_BLOCKED, "TMDB is limiting requests. The server will ask again later."
			return n, uint32(rl.RetryAfter.Seconds())
		}
		n.Status, n.Message = pluginv1.NoticeStatus_NOTICE_STATUS_ERROR, "TMDB could not be reached or gave an unexpected answer."
	}
	return n, 0
}

// notFoundNotice is the notice for a search or lookup that TMDB answered with nothing.
func notFoundNotice(message string) *pluginv1.Notice {
	return &pluginv1.Notice{Source: noticeSource, Status: pluginv1.NoticeStatus_NOTICE_STATUS_NOT_FOUND, Message: message}
}

// gaps collects what went missing during one request so the result can say so.
type gaps struct {
	notices []*pluginv1.Notice
	retry   uint32
}

// add records a failed side request. A cancelled request is not a gap: nobody is waiting for the answer.
func (g *gaps) add(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	n, retry := notice(err)
	if !slices.ContainsFunc(g.notices, func(x *pluginv1.Notice) bool { return x.Status == n.Status && x.Message == n.Message }) {
		g.notices = append(g.notices, n)
	}
	g.retry = max(g.retry, retry)
}

// apply hands the collected notices, the incomplete flag and the retry delay to set, which stores them on a result. It does nothing for an older host.
func (g *gaps) apply(set func(notices []*pluginv1.Notice, incomplete bool, retry uint32)) {
	if len(g.notices) == 0 || !hostShowsNotices() {
		return
	}
	set(g.notices[:min(len(g.notices), 32)], true, g.retry)
}
