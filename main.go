// Command tmdb is the official metadata provider plugin for TMDB (The Movie Database) (change request 0012, ADR 0008).
//
// The key is applied for by the user and entered in the admin console (API Read Access Token, sent as a Bearer token, never appearing in URLs or logs).
// Per the TMDB API terms: the Manifest declares the attribution; the plugin limits itself to 20 requests per second with 4 concurrent; on a 429 it returns RESOURCE_EXHAUSTED.
// The 6-month cache limit is handled by the server's refresh schedule (change request 0013); the plugin itself does not cache.
package main

import (
	"github.com/kryspal/plugin-sdk/sdk"
)

func main() {
	p := newProvider()
	sdk.Main(sdk.Config{Plugin: p, MetadataProvider: p, ImageProvider: p})
}
