# TMDB scraping research summary

- Survey date: 2026-10-05 (a read-only survey; this is the condensed version and keeps only the facts and decisions that concern this plugin).
- Licensing principle: for GPL/AGPL products only the public behavior and design are consulted, and source code is neither copied nor translated line by line; MoviePilot's filename test set and its identifier-word regex tables are also GPL content and must not be reused.
- Products consulted: tinyMediaManager (tMM, Apache-2.0), Kodi's two TMDB scrapers (GPL), the Jellyfin TMDb plugin (GPL-2.0), Emby's built-in MovieDb (closed source), Infuse (closed source), MoviePilot and its plugin collection (GPL-3.0), jellyfin-plugin-metashark (GPL-3.0), StrmAssistant (GPL-3.0, the open-source version is no longer updated), Radarr (GPL-3.0), and the official TMDB documentation.
- No actively maintained scraper designed for zh-TW was found; open-source products only convert toward Simplified. This plugin converts in both directions and treats Traditional and Simplified Chinese users equally.

## TMDB facts (design premises)

- **A missing translation returns the original text, not an empty string.** When the target language has no translation, `title`/`name` is the original title; an episode name is a placeholder such as the Chinese "Episode N" form or "Episode N"; only the overview is an empty string. So with a "fall back when the field is empty" approach the title fallback would never trigger. Measured against real TMDB: movie 1678 returns the original Japanese title (katakana "Gojira") under zh-TW.
- **translations**: each entry has `iso_639_1`, `iso_3166_1` and `data{title|name, overview, tagline, biography}`, and `append_to_response` returns all languages in one request. Movies, series, seasons, episodes, people and collections all support it. A person's `name` does not change with `language`; the translated names are in translations and `also_known_as`.
- **alternative_titles**: often more complete than translations; for example the Taiwan title of 1678 is registered only in the TW aliases.
- **Genre names**: TMDB's genre translations have only two-letter language codes and no regions (a TMDB staff member, 2023-02-22), so zh-TW and zh-HK always get Simplified.
- **Images**: since about 2025-10 they carry the region field `iso_3166_1`; `xx` means no text and null means unmarked. The official documentation still says "region variants are not supported", and the community also reports many wrongly marked regions, so the region can only be used for ordering and never for filtering. `include_image_language` accepts only two-letter language codes.
- **Search**: `/search/movie` matches the original title, translations and aliases; for movies `year` only affects ordering and does not filter (Jellyfin PR #17819); for series `first_air_date_year` filters out results that are one year off.
- **`/find`**: `external_source` supports `imdb_id`, `tvdb_id`, `wikidata_id` and others.
- **Whole-season request**: `/tv/{id}/season/{n}` contains, for every episode, the name, overview, still, crew and guest stars in that language.
- **release_dates types**: 1 premiere, 2 limited, 3 theatrical, 4 digital, 5 physical, 6 TV.
- **Episode groups**: `/tv/{id}/episode_groups`, `/tv/episode_group/{id}`; types 1 original air date, 2 absolute, 3 DVD, 4 digital, 5 story arc, 6 production, 7 TV.
- **changes**: `/movie/changes`, `/tv/changes`, `/person/changes`, at most 14 days at a time and 100 entries per page.
- **Rate limit**: about 40 per second, and a 429 must be honored; `append_to_response` takes at most 20 items.
- **Data defects** (measured): the zh-HK season name of Game of Thrones season 1 is the series title (the Chinese title of "Game of Thrones" in its zh-HK form); the aggregate_credits of a long-running series can reach hundreds of people (Jellyfin #17764 recorded 270,000 crew entries).

## Implemented (research list items 1 to 9)

| Item | Approach | Reference |
| --- | --- | --- |
| 1 Title fallback and placeholders | The main request appends `translations` (movies and series also `alternative_titles`), and judges by "whether that language has a translation"; placeholders (the Chinese ordinal forms for episode, chapter and season, "Episode N", bare digits) count as missing; when all are placeholders the episode name is left empty and the server or client shows "Episode N" | tMM, Kodi series scraper, StrmAssistant |
| 2 Chinese fallback chain and Simplified/Traditional conversion | The chain is produced by the plugin: zh-TW -> HK -> MO -> CN -> SG; zh-HK -> MO -> TW -> CN -> SG; zh-CN -> SG -> TW -> HK; followed by the requested fallback language and en-US. For titles, "the Chinese alias for the target region" is also inserted right after the target language. Results in the other script are converted with OpenCC: Simplified to Traditional `s2twp` (Taiwan wording) or `s2hk`, Traditional to Simplified `tw2sp` or `hk2s`. Can be turned off by a setting | Infuse, StrmAssistant; the direction is the opposite of MoviePilot |
| 3 Genre name table | Genre ID to en, zh-CN, zh-TW names (27 IDs across movies and series); zh-HK and zh-MO use zh-TW, and zh-SG uses zh-CN; adding a language only needs more data. IDs not in the table use TMDB's name, converted | MoviePilot (IDs only), StrmAssistant |
| 4 Search scoring | The plugin fills `SearchCandidate.score`; ID hints (tmdb, imdb, tvdb) score 1 directly; series searches carry no year; when the best score of a Chinese title is below 0.8, the search is made once more in the other script | tMM, Kodi, Jellyfin #17819, MoviePilot |
| 5 Images ordered by region | See "Image order" in the README; `include_image_language` adds `xx`; `Image.language` is filled with BCP 47 (`zh-TW`); SVG is skipped | Jellyfin #18150, Infuse 8.4.x |
| 6 Episodes from the whole season | Seasons and episodes both come from the whole-season response, cached by (series, season, language) for 30 minutes, at most 64 entries; concurrent requests are merged; errors are not cached | Kodi series scraper, tMM, MoviePilot |
| 7 Cast and crew tidy-up | Series cast is ordered by episode appearances, with the role taken from the one with the most appearances; the cast limit is configurable (default 50); crew is de-duplicated by "person, job"; at most 5 producers | Jellyfin #17716, Kodi series scraper |
| 8 Person names | Same-language translation (those from other regions converted) -> a Chinese alias in `also_known_as` (the script judged by conversion, target script first) -> main name | MoviePilot `personmeta` (opposite direction), StrmAssistant |
| 9 Certification | Release type priority 3, 4, 5, 6, 1, 2; the certification country/region is configurable, defaulting to the region of the target language and then falling back to the US | Jellyfin, Kodi |

Also done: a retry once on 5xx and dropped connections (after waiting 0.5 to 1 second); the README notes that `https://api.tmdb.org/3` can serve as an alternate host.

Differences from the research recommendations:

- Simplified/Traditional conversion uses `pluginapi/hanconv` (the OpenCC dictionaries, Apache-2.0, embedded; it reproduces the output of `github.com/liuzl/gocc`, which this note originally chose and which was dropped because its dependency chain includes a GPL-2.0 module), rather than adding longbridge/opencc (its module path does not match the repo).
- A season name is taken only when it is in the same language and does not equal the series title in any language; otherwise it is generated according to the target language (the Chinese "Season N" and "Specials", "Season N", "Specials"), to avoid the zh-HK defect above.
- When the original text is in the target language (for example a Japanese movie in a Japanese library), a missing translation uses the original title and does not switch to the English title.

## Not implemented

- **Item 10 (needs a protocol change)**: trailers (`videos`), `GetItemRequest.episode_group_id` (to specify an episode group; it must specify the exact group ID, not just the type), and `ListChanged` (the changes API, used with incremental refresh by how recent the content is; the refresh schedule belongs to the server).
- **Candidate detail confirmation** (MoviePilot fetches details for every candidate to compare aliases): N+1 requests; left until the "recognition" interface needs it.
- **Removing the Chinese "series" marker from collection names**: an appearance preference, kept by default, consistent with Emby.
- **keywords to tags**: TMDB keywords are English only, so the value is low.
- **A backdrop with text as Thumb**: the protocol has `IMAGE_TYPE_THUMB`, but TMDB does not mark which backdrops carry the title; first confirm how Emby clients use Thumb, then decide.
- **Outside the plugin**: the `tmdbid-` marker in filenames (server), user-defined identifier words (server), anime season mapping (a Bangumi provider), and IMDb/Trakt ratings (separate providers).

## Practices to avoid (lessons from each product)

- Truncating zh-TW to `zh` (MetaShark still does so; Jellyfin only fixed it in 2026-02).
- Looking only at the image language and not the region (zh-TW and zh-CN posters get mixed).
- No fallback for text (a known Jellyfin limitation).
- Taking only "the first episode group of a type" (Jellyfin, MetaShark).
- Fetching details for every candidate (N+1 requests).
- Hard-coding a shared API key in the source (Kodi, Jellyfin); this plugin is bring-your-own-key.
- Unpredictable fallback rules after a rework (Infuse 8.4.x first picked the wrong Chinese variant, then picked a textless poster): this plugin's rules are fixed by unit tests and written into the README.
- Changing a person's name so that the link between the person and the items breaks (Jellyfin): this project keys people by TMDB ID, so it is not affected.
