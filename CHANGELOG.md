# Changelog

All notable changes to this plugin are listed here. The version in `VERSION` is the single source of the version number; a published version is never changed.

## 2.5.2

- Built with plugin SDK v0.5.0. No functional change.

## 2.5.1

- A candidate whose name is only the beginning of the searched title no longer scores almost as high as the candidate whose name is identical: when an identical match exists, prefix matches are held at 0.85 (reason `title.shadowed`), so the server applies the identical one instead of calling the pair ambiguous. Without an identical match nothing changes.
- Built with plugin SDK v0.2.0. Needs server 0.9.2 or newer.

## 2.5.0

- Renames the advanced setting `max_backdrops` to `max_stills`, "Maximum stills", and raises its default from 20 to 30 (still 0 to 200, 0 means all). The plugin now returns that many stills plus one more backdrop, the first, which the server uses as the background picture.
- Built with plugin SDK v0.2.0. Needs server 0.9.2 or newer.

## 2.4.0

- Reports what happened at TMDB to servers that show it: a search that finds nothing says "no match", a failed request for a title's original-language images is reported (rate limited, timed out, error) and the result is marked incomplete so the server asks again later (after TMDB's `Retry-After` when it gave one), and a person without photos says so. The notices are fixed sentences and never contain the token. Older servers get exactly the results they got before.
- Built with plugin SDK v0.2.0. Needs server 0.9.2 or newer.

## 2.3.1

- Bounds the token check in `Configure` to 5 seconds, so a hanging TMDB can no longer delay startup or a settings save for up to 40 seconds. The settings are still accepted when TMDB cannot be reached.

## 2.3.0

- Adds the advanced settings `backdrop_size`, `poster_size` and `profile_size`: choose a smaller download size for backdrops (`w1280`), posters (`w780`) and person photos (`h632`) to save bandwidth and disk space. The default stays `original`, so existing installations are unchanged. A change only affects titles scraped or refreshed afterwards.

## 2.2.1

- Rebuilt with Go 1.27.2 and golang.org/x/net v0.60.0 (security fixes).
- Includes the changes since the last published catalog version (2.1.9): the `images.person` capability (all profile photos TMDB has for a person), and the documentation and source cleanup for the public repository.

## 2.2.0

- Adds the `images.person` capability: all profile photos TMDB has for a person.
- Metadata for movies, series (seasons and episodes), people, collections and trailers.
- Development history before this release lives in the Kryspal Media main repository; this repository starts from this version.
