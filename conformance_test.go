package main

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kryspal/plugin-sdk/plugintest"
)

// TestConformance builds the plugin and runs the SDK conformance suite against the binary, with a fake TMDB over HTTPS (the plugin only
// accepts an https base URL, and trusts the fake's certificate through SSL_CERT_FILE).
func TestConformance(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "plugin")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	up := plugintest.NewTLSUpstream(t)
	up.Handle("GET /authentication", plugintest.JSON(http.StatusOK, map[string]any{"success": true}))
	up.Handle("GET /search/person", plugintest.JSON(http.StatusOK, map[string]any{"results": []any{
		map[string]any{"id": 1001, "name": "Sample Person", "gender": 2, "profile_path": "/sample.jpg"},
	}}))
	plugintest.Run(t, bin, plugintest.Options{
		Config:       map[string]string{"api_token": "0123456789abcdef0123456789abcdef", "base_url": up.URL},
		Env:          up.Env(),
		PersonSearch: &plugintest.PersonSearchCase{Name: "Sample Person"},
	})
}
