package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	pluginID = "tmdb"

	keyToken        = "api_token"
	keyIncludeAdult = "include_adult"
	keyBaseURL      = "base_url"
	keyMaxCast      = "max_cast"
	keyMaxStills    = "max_stills"
	keyCertCountry  = "certification_country"
	keyConvertZh    = "convert_chinese"
	keyTrimSeries   = "trim_collection_suffix"

	// authCheckTimeout bounds the token check in Configure (protocol: verification takes no more than a few seconds).
	authCheckTimeout = 5 * time.Second

	maxCastLimit = 500
	// defaultMaxStills is the default cap on the number of stills (setting max_stills; 0 means all); maxStillsLimit is the upper bound of the setting.
	// TMDB can hold over a hundred stills for a popular work, and the Emby-compatible API attaches BackdropImageTags to every item, so too many tags would bloat list responses.
	defaultMaxStills = 30
	maxStillsLimit   = 200

	// attributionText is the statement TMDB requires (the English original, the same in every language; the logo must be less prominent than this product's own logo, see README).
	// It is only placed in Manifest.attribution.text (shown in the attribution row of the detail page) and not repeated in the description.
	attributionText = "This product uses the TMDB API but is not endorsed or certified by TMDB."
	attributionLink = "https://www.themoviedb.org/"
)

// logoSVG is the official TMDB "Primary short (blue)" logo; the original file is not modified (colors and proportions follow TMDB's usage guidelines), source in the README.
// It is placed in the Manifest as a data: URI so the admin console does not need to reach TMDB to show it.
//
//go:embed logo/tmdb-primary-short-blue.svg
var logoSVG []byte

var logoDataURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(logoSVG)

// certAuto is the option value for "auto" in the certification country menu (an empty string cannot be an option in a form).
const certAuto = "auto"

var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

// versionFile is the single source of the plugin version: the Manifest version.
//
//go:embed VERSION
var versionFile string

func version() string { return strings.TrimSpace(versionFile) }

type provider struct {
	pluginv1.UnimplementedPluginServer
	pluginv1.UnimplementedMetadataProviderServer
	pluginv1.UnimplementedImageProviderServer

	mu           sync.Mutex
	client       *tmdbClient // nil: not yet configured
	includeAdult bool
	maxCast      int         // cap on cast members; 0 means unlimited
	maxStills    int         // cap on stills; 0 means unlimited
	certCountry  string      // certification country (two uppercase letters); an empty string means follow the target language's region
	sizes        imageSizes  // download sizes for posters, backdrops and profile photos (empty means original)
	trimSeries   atomic.Bool // strip the trailing Chinese "(series)" or "Collection" suffix from collection names (trim_collection_suffix)
	conv         converter   // for output; setting convert_chinese can turn it off
	fold         converter   // for search matching; always on
	seasons      seasonCache
	aliases      aliasCache // candidate aliases and translated names for search
	infos        infoCache  // candidate details for hinted search and absolute episode numbers (aliases, release date, runtime, season episode counts)
	searches     searchCache
	// newClient creates the TMDB client; tests can replace it.
	newClient func(baseURL, token string, log *slog.Logger) *tmdbClient
	log       *slog.Logger // writes to stderr when nil (the SDK forwards stderr line by line into the server log)
}

// capabilities are the capabilities declared in the Manifest.
func capabilities() []string {
	caps := []string{pluginv1.CapabilityMetadataMovie, pluginv1.CapabilityMetadataSeries, pluginv1.CapabilityMetadataPerson,
		pluginv1.CapabilityMetadataCollection, pluginv1.CapabilityMetadataTrailers, pluginv1.CapabilityMetadataPersonSearch,
		pluginv1.CapabilityMetadataSeriesAbsolute, pluginv1.CapabilityMetadataSearchHints, pluginv1.CapabilityImagesPerson}
	return caps
}

func newProvider() *provider {
	return &provider{newClient: newClient, maxCast: defaultMaxCast, maxStills: defaultMaxStills}
}

// Describe implements pluginv1.PluginServer.
func (p *provider) Describe(_ context.Context, req *pluginv1.DescribeRequest) (*pluginv1.Manifest, error) {
	t := messages.For(req.GetLanguage())
	return &pluginv1.Manifest{
		Id:              pluginID,
		Name:            "TMDB",
		Description:     t.T("desc"),
		Version:         version(),
		ProtocolVersion: pluginv1.ProtocolVersion,
		Capabilities:    capabilities(),
		ConfigGroups:    []*pluginv1.ConfigGroup{{Key: "advanced", Title: t.T("groupAdvanced")}},
		ConfigFields: []*pluginv1.ConfigField{
			{
				Key: keyToken, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_SECRET, Required: true,
				Label:       t.T("tokenLabel"),
				Description: t.T("tokenHelp"),
				Link:        "https://www.themoviedb.org/settings/api", LinkLabel: t.T("tokenLink"),
			},
			{
				Key: keyIncludeAdult, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_BOOL, DefaultValue: "false",
				Label: t.T("adultLabel"),
			},
			{
				Key: keyBaseURL, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_STRING, Group: "advanced",
				Label:       t.T("baseURLLabel"),
				Description: t.T("baseURLHelp"),
				Placeholder: defaultBaseURL,
			},
			{
				Key: keyConvertZh, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_BOOL, DefaultValue: "true", Group: "advanced",
				Label:       t.T("convertLabel"),
				Description: t.T("convertHelp"),
			},
			{
				Key: keyTrimSeries, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_BOOL, DefaultValue: "false", Group: "advanced",
				Label:       t.T("trimLabel"),
				Description: t.T("trimHelp"),
			},
			{
				Key: keyMaxCast, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_INT, DefaultValue: strconv.Itoa(defaultMaxCast), Group: "advanced",
				Min: proto.Int64(0), Max: proto.Int64(maxCastLimit),
				Label:       t.T("maxCastLabel"),
				Description: t.T("maxCastHelp"),
			},
			{
				Key: keyMaxStills, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_INT, DefaultValue: strconv.Itoa(defaultMaxStills), Group: "advanced",
				Min: proto.Int64(0), Max: proto.Int64(maxStillsLimit),
				Label:       t.T("maxStillsLabel"),
				Description: t.T("maxStillsHelp"),
			},
			sizeField(keyBackdropSize, "backdropSize", backdropSizes, t),
			sizeField(keyPosterSize, "posterSize", posterSizes, t),
			sizeField(keyProfileSize, "profileSize", profileSizes, t),
			{
				Key: keyCertCountry, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_SELECT, Group: "advanced",
				Label:        t.T("certLabel"),
				Description:  t.T("certHelp"),
				DefaultValue: certAuto,
				Options: []*pluginv1.ConfigOption{
					{Value: certAuto, Label: t.T("certAuto")},
					{Value: "TW", Label: t.T("country.TW")},
					{Value: "HK", Label: t.T("country.HK")},
					{Value: "MO", Label: t.T("country.MO")},
					{Value: "CN", Label: t.T("country.CN")},
					{Value: "SG", Label: t.T("country.SG")},
					{Value: "MY", Label: t.T("country.MY")},
					{Value: "JP", Label: t.T("country.JP")},
					{Value: "KR", Label: t.T("country.KR")},
					{Value: "TH", Label: t.T("country.TH")},
					{Value: "IN", Label: t.T("country.IN")},
					{Value: "US", Label: t.T("country.US")},
					{Value: "CA", Label: t.T("country.CA")},
					{Value: "MX", Label: t.T("country.MX")},
					{Value: "BR", Label: t.T("country.BR")},
					{Value: "GB", Label: t.T("country.GB")},
					{Value: "IE", Label: t.T("country.IE")},
					{Value: "DE", Label: t.T("country.DE")},
					{Value: "FR", Label: t.T("country.FR")},
					{Value: "IT", Label: t.T("country.IT")},
					{Value: "ES", Label: t.T("country.ES")},
					{Value: "NL", Label: t.T("country.NL")},
					{Value: "SE", Label: t.T("country.SE")},
					{Value: "RU", Label: t.T("country.RU")},
					{Value: "AU", Label: t.T("country.AU")},
					{Value: "NZ", Label: t.T("country.NZ")},
				},
			},
		},
		Attribution:          &pluginv1.Attribution{Text: attributionText, LogoUrl: logoDataURI, Link: attributionLink},
		SuggestedConcurrency: maxConcurrent,
	}, nil
}

// Configure implements pluginv1.PluginServer. It validates the token with /authentication: a field error is returned only when TMDB explicitly rejects it (401);
// when TMDB is unreachable the settings are still accepted, otherwise if the network is not yet ready when the server boots the plugin would stay in awaiting configuration forever.
func (p *provider) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	t := messages.For(req.GetLanguage())
	v := req.GetValues()
	resp := &pluginv1.ConfigureResponse{FieldErrors: map[string]string{}}
	token := strings.TrimSpace(v[keyToken])
	if token == "" {
		resp.FieldErrors[keyToken] = t.T("errRequired")
	}
	base := strings.TrimSpace(v[keyBaseURL])
	if base == "" {
		base = defaultBaseURL
	}
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" || u.Host == "" {
		resp.FieldErrors[keyBaseURL] = t.T("errHTTPS")
	}
	maxCast := defaultMaxCast
	if raw := strings.TrimSpace(v[keyMaxCast]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxCastLimit {
			resp.FieldErrors[keyMaxCast] = t.T("errMaxCast")
		}
		maxCast = n
	}
	maxStills := defaultMaxStills
	if raw := strings.TrimSpace(v[keyMaxStills]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxStillsLimit {
			resp.FieldErrors[keyMaxStills] = t.T("errMaxStills")
		}
		maxStills = n
	}
	var sizes imageSizes
	for _, f := range []struct {
		key     string
		offered []string
		dst     *string
	}{{keyPosterSize, posterSizes, &sizes.poster}, {keyBackdropSize, backdropSizes, &sizes.backdrop}, {keyProfileSize, profileSizes, &sizes.profile}} {
		raw := strings.TrimSpace(v[f.key])
		if !validSize(raw, f.offered) {
			resp.FieldErrors[f.key] = t.T("errImageSize")
		}
		*f.dst = raw
	}
	country := strings.ToUpper(strings.TrimSpace(v[keyCertCountry]))
	if strings.EqualFold(country, certAuto) {
		country = ""
	}
	if country != "" && !countryRE.MatchString(country) {
		resp.FieldErrors[keyCertCountry] = t.T("errCountry")
	}
	if len(resp.FieldErrors) > 0 {
		return resp, nil
	}
	if token != "" && classifyToken(token) == tokenInvalid {
		resp.FieldErrors[keyToken] = t.T("errTokenFormat")
		return resp, nil
	}
	c := p.newClient(base, token, p.log)
	var auth struct {
		Success bool `json:"success"`
	}
	// Verification must not hold up startup: cap it (the client's own timeout is 20 s per attempt, with one retry).
	vctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()
	switch err := c.get(vctx, "/authentication", nil, &auth); {
	case errors.Is(err, errUnauthorized):
		resp.FieldErrors[keyToken] = t.T("errTokenRejected")
		return resp, nil
	case err != nil:
		c.log.Warn("could not verify the TMDB token; accepting it for now", slog.String("error", err.Error()))
	}
	p.mu.Lock()
	p.client, p.includeAdult = c, v[keyIncludeAdult] == "true"
	p.maxCast, p.certCountry, p.maxStills, p.sizes = maxCast, country, maxStills, sizes
	p.mu.Unlock()
	p.conv.setOff(v[keyConvertZh] == "false")
	p.trimSeries.Store(v[keyTrimSeries] == "true")
	return resp, nil
}

// ready returns the configured client; when not yet configured it returns FAILED_PRECONDITION.
func (p *provider) ready() (*tmdbClient, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		return nil, false, sdk.NeedsConfiguration("TMDB API token is not configured")
	}
	return p.client, p.includeAdult, nil
}

// upstream converts an error from the TMDB client c into a protocol error. A 401 means the token is no longer valid and the plugin goes back to awaiting configuration.
// It only clears when c is still the current client: a 401 from an old request must not clear a new token just swapped in by Configure.
func (p *provider) upstream(c *tmdbClient, err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errUnauthorized):
		p.mu.Lock()
		if p.client == c {
			p.client = nil
		}
		p.mu.Unlock()
		return sdk.NeedsConfiguration(errUnauthorized.Error())
	case errors.Is(err, errNotFound):
		return sdk.NotFound(what + " not found on TMDB")
	}
	if _, ok := errors.AsType[*sdk.RateLimitError](err); ok {
		return err // the SDK converts it to RESOURCE_EXHAUSTED and attaches retry-after
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	// connection error, 5xx, malformed response: upstream is temporarily unavailable, and the server retries later.
	return sdk.Unavailable(err.Error())
}
