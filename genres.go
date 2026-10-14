package main

// Genre name table: TMDB's genre translations only use two-letter language codes with no region, so zh-TW gets Simplified Chinese (stated by a TMDB staff member, 2023-02-22),
// so the plugin looks up its own name table by genre ID. To add a language, add that language's name to each row; when there is no match, use the name TMDB returned
// (converted to the target script for Chinese).
//
// Keys are BCP 47: a full language-region (zh-TW) or just a language (en). See genreName for the lookup order.

var genreNames = map[int]map[string]string{
	// shared by movies and series
	16:    {"en": "Animation", "zh-CN": "动画", "zh-TW": "動畫"},
	35:    {"en": "Comedy", "zh-CN": "喜剧", "zh-TW": "喜劇"},
	80:    {"en": "Crime", "zh-CN": "犯罪", "zh-TW": "犯罪"},
	99:    {"en": "Documentary", "zh-CN": "纪录", "zh-TW": "紀錄"},
	18:    {"en": "Drama", "zh-CN": "剧情", "zh-TW": "劇情"},
	10751: {"en": "Family", "zh-CN": "家庭", "zh-TW": "家庭"},
	9648:  {"en": "Mystery", "zh-CN": "悬疑", "zh-TW": "懸疑"},
	37:    {"en": "Western", "zh-CN": "西部", "zh-TW": "西部"},
	// movies
	28:    {"en": "Action", "zh-CN": "动作", "zh-TW": "動作"},
	12:    {"en": "Adventure", "zh-CN": "冒险", "zh-TW": "冒險"},
	14:    {"en": "Fantasy", "zh-CN": "奇幻", "zh-TW": "奇幻"},
	36:    {"en": "History", "zh-CN": "历史", "zh-TW": "歷史"},
	27:    {"en": "Horror", "zh-CN": "恐怖", "zh-TW": "恐怖"},
	10402: {"en": "Music", "zh-CN": "音乐", "zh-TW": "音樂"},
	10749: {"en": "Romance", "zh-CN": "爱情", "zh-TW": "愛情"},
	878:   {"en": "Science Fiction", "zh-CN": "科幻", "zh-TW": "科幻"},
	10770: {"en": "TV Movie", "zh-CN": "电视电影", "zh-TW": "電視電影"},
	53:    {"en": "Thriller", "zh-CN": "惊悚", "zh-TW": "驚悚"},
	10752: {"en": "War", "zh-CN": "战争", "zh-TW": "戰爭"},
	// series
	10759: {"en": "Action & Adventure", "zh-CN": "动作冒险", "zh-TW": "動作冒險"},
	10762: {"en": "Kids", "zh-CN": "儿童", "zh-TW": "兒童"},
	10763: {"en": "News", "zh-CN": "新闻", "zh-TW": "新聞"},
	10764: {"en": "Reality", "zh-CN": "真人秀", "zh-TW": "實境"},
	10765: {"en": "Sci-Fi & Fantasy", "zh-CN": "科幻奇幻", "zh-TW": "科幻奇幻"},
	10766: {"en": "Soap", "zh-CN": "肥皂剧", "zh-TW": "肥皂劇"},
	10767: {"en": "Talk", "zh-CN": "脱口秀", "zh-TW": "脫口秀"},
	10768: {"en": "War & Politics", "zh-CN": "战争政治", "zh-TW": "戰爭政治"},
}

// genreName returns the name of a genre ID in the target language: it looks up, in order, the full language-region, the Chinese region with the same script (zh-HK and zh-MO use zh-TW,
// zh-SG uses zh-CN), and the language-only key. When none exists it returns the name TMDB gave (fallback), converted to the target script for Chinese.
func (p *provider) genreName(id int, fallback string, target locale) string {
	names := genreNames[id]
	keys := []string{target.String()}
	switch target.script() {
	case "Hant":
		keys = append(keys, "zh-TW")
	case "Hans":
		keys = append(keys, "zh-CN")
	}
	keys = append(keys, target.lang)
	for _, k := range keys {
		if n, ok := names[k]; ok {
			return n
		}
	}
	// TMDB's Chinese genre names are Simplified (no regional variants).
	return p.conv.convert(fallback, locale{lang: "zh", region: "CN"}, target)
}
