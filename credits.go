package main

import (
	"cmp"
	"slices"
	"strconv"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

const (
	// defaultMaxCast is the default cap on the number of cast members (setting max_cast). A series' aggregate_credits can reach hundreds of people.
	defaultMaxCast = 50
	// maxProducers is the cap on producers: series-level Producer and Executive Producer lists often run to dozens of people.
	maxProducers = 5
)

// credits converts the cast and crew:
//
//   - Cast: for a series (aggregate_credits), in descending order of episode count, with the role taken from the one with the most appearances; movies and episodes keep TMDB's order. At most maxCast people.
//   - Episode guest stars: deduplicated, with sequential ordering.
//   - Crew: directors, writers (Writing department), producers (at most maxProducers people), composers; deduplicated by person and role.
func credits(c *tmdbCredits, guests []tmdbCast, maxCast int) []*pluginv1.Credit {
	var out []*pluginv1.Credit
	person := func(id int, name string, kind pluginv1.PersonKind, role string, order int, profile string, g int) *pluginv1.Credit {
		return &pluginv1.Credit{PersonId: strconv.Itoa(id), Name: name, Kind: kind, Role: role, Order: int32(order),
			ImageUrl: imageURL(profile), Gender: gender(g), ProviderIds: map[string]string{"tmdb": strconv.Itoa(id)}}
	}
	var cast []tmdbCast
	var crew []tmdbCrew
	if c != nil {
		cast, crew = c.Cast, c.Crew
		guests = append(slices.Clone(c.GuestStars), guests...)
	}
	cast = slices.Clone(cast)
	aggregate := slices.ContainsFunc(cast, func(x tmdbCast) bool { return len(x.Roles) > 0 })
	if aggregate {
		slices.SortStableFunc(cast, func(a, b tmdbCast) int { return cmp.Compare(b.TotalEpisodes, a.TotalEpisodes) })
	}
	seen := map[int]bool{}
	n := 0
	for _, x := range cast {
		if seen[x.ID] || maxCast > 0 && n >= maxCast {
			continue
		}
		seen[x.ID] = true
		role := x.Character
		if aggregate {
			best := -1
			for _, r := range x.Roles {
				if r.EpisodeCount > best && r.Character != "" {
					role, best = r.Character, r.EpisodeCount
				}
			}
		}
		out = append(out, person(x.ID, x.Name, pluginv1.PersonKind_PERSON_KIND_ACTOR, role, n, x.ProfilePath, x.Gender))
		n++
	}
	seenGuest := map[int]bool{}
	g := 0
	for _, x := range guests {
		if seenGuest[x.ID] || seen[x.ID] {
			continue
		}
		seenGuest[x.ID] = true
		out = append(out, person(x.ID, x.Name, pluginv1.PersonKind_PERSON_KIND_GUEST_STAR, x.Character, g, x.ProfilePath, x.Gender))
		g++
	}
	type key struct {
		id   int
		kind pluginv1.PersonKind
	}
	seenCrew := map[key]bool{}
	order := map[pluginv1.PersonKind]int{}
	for _, x := range crew {
		jobs := []string{x.Job}
		for _, j := range x.Jobs {
			jobs = append(jobs, j.Job)
		}
		for _, job := range jobs {
			kind := crewKind(job, x.Department)
			if kind == pluginv1.PersonKind_PERSON_KIND_UNSPECIFIED || seenCrew[key{x.ID, kind}] {
				continue
			}
			if kind == pluginv1.PersonKind_PERSON_KIND_PRODUCER && order[kind] >= maxProducers {
				continue
			}
			seenCrew[key{x.ID, kind}] = true
			out = append(out, person(x.ID, x.Name, kind, "", order[kind], x.ProfilePath, x.Gender))
			order[kind]++
		}
	}
	return out
}

func crewKind(job, department string) pluginv1.PersonKind {
	switch {
	case job == "":
		return pluginv1.PersonKind_PERSON_KIND_UNSPECIFIED
	case job == "Director":
		return pluginv1.PersonKind_PERSON_KIND_DIRECTOR
	case job == "Executive Producer" || job == "Producer":
		return pluginv1.PersonKind_PERSON_KIND_PRODUCER
	case job == "Original Music Composer" || job == "Music" || job == "Composer":
		return pluginv1.PersonKind_PERSON_KIND_COMPOSER
	case department == "Writing": // in aggregate_credits the department belongs to the whole person, not to each job: look at the job first
		return pluginv1.PersonKind_PERSON_KIND_WRITER
	}
	return pluginv1.PersonKind_PERSON_KIND_UNSPECIFIED
}

// releaseTypeOrder is the priority order of release types when picking a certification: theatrical (3), digital (4), physical (5), TV (6), premiere (1), limited (2).
var releaseTypeOrder = []int{3, 4, 5, 6, 1, 2}

// officialRating takes the certification for country, falling back to the US. For movies it picks the first release with a certification in the release-type priority order; for series it looks at content_ratings.
func officialRating(it tmdbItem, country string) string {
	pickRating := func(c string) string {
		if rd := it.ReleaseDates; rd != nil {
			for _, typ := range releaseTypeOrder {
				for _, r := range rd.Results {
					if r.ISO != c {
						continue
					}
					for _, d := range r.Dates {
						if d.Type == typ && d.Certification != "" {
							return d.Certification
						}
					}
				}
			}
		}
		if cr := it.ContentRatings; cr != nil {
			for _, r := range cr.Results {
				if r.ISO == c && r.Rating != "" {
					return r.Rating
				}
			}
		}
		return ""
	}
	return first(pickRating(country), pickRating("US"))
}
