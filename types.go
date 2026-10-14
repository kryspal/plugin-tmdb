package main

// TMDB responses (only the fields that are used).

type tmdbGenre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type tmdbNamed struct {
	Name string `json:"name"`
}

type tmdbCountry struct {
	ISO  string `json:"iso_3166_1"`
	Name string `json:"name"`
}

type tmdbCast struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character"`
	Order       int    `json:"order"`
	ProfilePath string `json:"profile_path"`
	Gender      int    `json:"gender"` // 0 unknown, 1 female, 2 male, 3 non-binary
	// Only in a series' aggregate_credits: roles and episode counts.
	Roles []struct {
		Character    string `json:"character"`
		EpisodeCount int    `json:"episode_count"`
	} `json:"roles"`
	TotalEpisodes int `json:"total_episode_count"`
}

type tmdbCrew struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Job         string `json:"job"`
	Department  string `json:"department"`
	ProfilePath string `json:"profile_path"`
	Gender      int    `json:"gender"`
	// Only in a series' aggregate_credits: one person with several jobs.
	Jobs []struct {
		Job string `json:"job"`
	} `json:"jobs"`
}

type tmdbCredits struct {
	Cast       []tmdbCast `json:"cast"`
	Crew       []tmdbCrew `json:"crew"`
	GuestStars []tmdbCast `json:"guest_stars"`
}

type tmdbExternalIDs struct {
	IMDb      string `json:"imdb_id"`
	TVDB      int    `json:"tvdb_id"`
	Wikidata  string `json:"wikidata_id"`
	Facebook  string `json:"facebook_id"`
	Instagram string `json:"instagram_id"`
	Twitter   string `json:"twitter_id"`
}

// tmdbAltTitle is one entry of alternative_titles (a movie's list is in titles, a series' is in results).
type tmdbAltTitle struct {
	Region string `json:"iso_3166_1"`
	Title  string `json:"title"`
}

type tmdbAltTitles struct {
	Titles  []tmdbAltTitle `json:"titles"`
	Results []tmdbAltTitle `json:"results"`
}

func (a *tmdbAltTitles) all() []tmdbAltTitle {
	if a == nil {
		return nil
	}
	return append(append([]tmdbAltTitle(nil), a.Titles...), a.Results...)
}

// tmdbItem covers the details of movies and series (which fields exist depends on the kind).
type tmdbItem struct {
	ID            int    `json:"id"`
	Title         string `json:"title"` // movie
	Name          string `json:"name"`  // series
	OriginalTitle string `json:"original_title"`
	OriginalName  string `json:"original_name"`
	OriginalLang  string `json:"original_language"`
	Overview      string `json:"overview"`
	Tagline       string `json:"tagline"`
	ReleaseDate   string `json:"release_date"`   // movie
	FirstAirDate  string `json:"first_air_date"` // series
	LastAirDate   string `json:"last_air_date"`
	Status        string `json:"status"` // series: Returning Series, Ended, Canceled…
	// A series' season count and each season's episode count (included in /tv/{id}); used for shape matching and absolute episode numbers in hinted search.
	NumberOfSeasons int `json:"number_of_seasons"`
	Seasons         []struct {
		SeasonNumber int `json:"season_number"`
		EpisodeCount int `json:"episode_count"`
	} `json:"seasons"`
	OriginCountry  []string      `json:"origin_country"`
	VoteAverage    float64       `json:"vote_average"`
	VoteCount      int64         `json:"vote_count"`
	Runtime        int           `json:"runtime"` // minutes
	EpisodeRunTime []int         `json:"episode_run_time"`
	IMDbID         string        `json:"imdb_id"`
	Genres         []tmdbGenre   `json:"genres"`
	Companies      []tmdbNamed   `json:"production_companies"`
	Networks       []tmdbNamed   `json:"networks"`
	Countries      []tmdbCountry `json:"production_countries"`
	PosterPath     string        `json:"poster_path"`
	BackdropPath   string        `json:"backdrop_path"`
	Collection     *struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"belongs_to_collection"`
	Credits      *tmdbCredits      `json:"credits"`
	Aggregate    *tmdbCredits      `json:"aggregate_credits"` // series: credits across all seasons
	Images       *tmdbImages       `json:"images"`
	ExternalIDs  *tmdbExternalIDs  `json:"external_ids"`
	Translations *tmdbTranslations `json:"translations"`
	AltTitles    *tmdbAltTitles    `json:"alternative_titles"`
	Videos       *tmdbVideos       `json:"videos"`
	ReleaseDates *struct {
		Results []struct {
			ISO   string `json:"iso_3166_1"`
			Dates []struct {
				Certification string `json:"certification"`
				Type          int    `json:"type"`
				ReleaseDate   string `json:"release_date"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
	ContentRatings *struct {
		Results []struct {
			ISO    string `json:"iso_3166_1"`
			Rating string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
}

func (it *tmdbItem) translations() []tmdbTranslation {
	if it.Translations == nil {
		return nil
	}
	return it.Translations.Translations
}
