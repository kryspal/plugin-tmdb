# Changelog

All notable changes to this plugin are listed here. The version in `VERSION` is the single source of the version number; a published version is never changed.

## 2.3.0

- Adds the advanced settings `backdrop_size`, `poster_size` and `profile_size`: choose a smaller download size for backdrops (`w1280`), posters (`w780`) and person photos (`h632`) to save bandwidth and disk space. The default stays `original`, so existing installations are unchanged. A change only affects titles scraped or refreshed afterwards.

## 2.2.1

- Rebuilt with Go 1.27.2 and golang.org/x/net v0.60.0 (security fixes).
- Includes the changes since the last published catalog version (2.1.9): the `images.person` capability (all profile photos TMDB has for a person), and the documentation and source cleanup for the public repository.

## 2.2.0

- Adds the `images.person` capability: all profile photos TMDB has for a person.
- Metadata for movies, series (seasons and episodes), people, collections and trailers.
- Development history before this release lives in the Kryspal Media main repository; this repository starts from this version.
