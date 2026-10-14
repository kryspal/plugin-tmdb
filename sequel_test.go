package main

import (
	"context"
	"testing"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

func TestSequelNumber(t *testing.T) {
	for title, want := range map[string]int{
		"Zorblax Courier II": 2, "Zorblax Courier 2": 2, "Zorblax Courier III: Fog": 3, "Zorblax Courier Ⅱ": 2, "星河信使2": 2, "星河信使2：诡影游戏": 2,
		"星河信使第二部": 2, "星河信使 第3部": 3, "Fast Courier 10": 10,
		"Zorblax Courier": 0, "星河信使": 0, "1917": 0, "Zorblax Courier 1": 0, "Zorblax Courier 2049": 0, "Zorblax Courier: Part II": 0, "": 0,
	} {
		if got := sequelNumber(title); got != want {
			t.Errorf("sequelNumber(%q) = %d, want %d", title, got, want)
		}
	}
}

func TestSequelGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		names []string
		in    float64
		want  float64
	}{
		{"no marker in the query", "Zorblax Courier", []string{"Zorblax Courier"}, 1, 1},
		{"same marker", "Zorblax Courier II", []string{"Zorblax Courier II"}, 1, 1},
		{"roman and arabic are the same", "Zorblax Courier II", []string{"Zorblax Courier 2"}, 1, 1},
		{"one of the aliases has it", "星河信使II", []string{"星河信使", "Zorblax Courier", "星河信使2"}, 1, 1},
		{"the candidate is the first film", "Zorblax Courier II", []string{"Zorblax Courier", "Zorblax Courier"}, 0.95, noSequelCap},
		{"the candidate is another sequel", "Zorblax Courier II", []string{"Zorblax Courier 3"}, 1, otherSequelCap},
		{"a low score stays low", "Zorblax Courier II", []string{"Zorblax Courier"}, 0.4, 0.4},
	} {
		if got := sequelGuard(tc.query, tc.names, tc.in); got != tc.want {
			t.Errorf("%s: sequelGuard = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Movies: when the query carries a sequel number, a first film with the same title must not get an auto-apply score; a real sequel scores 1.0 as usual.
func TestMovieSearchDoesNotAutoApplyTheFirstFilmForASequel(t *testing.T) {
	f := newAliasTMDB(t)
	f.results["movie|Zorblax Courier II"] = []map[string]any{
		result(10, "Zorblax Courier", "Zorblax Courier", "2002-12-12"),
		result(11, "Zorblax Courier II", "Zorblax Courier II", "2003-09-04"),
	}
	f.results["movie|Zorblax Courier 2"] = []map[string]any{result(10, "Zorblax Courier", "Zorblax Courier", "2002-12-12")}
	p := f.provider(t)
	ctx := context.Background()
	req := &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Courier II", Year: 2003}
	r, err := p.Search(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if scoreOf(r, 11) != 1 || scoreOf(r, 10) > noSequelCap || r.GetCandidates()[0].GetId() != "11" {
		t.Errorf("scores: sequel %v first film %v", scoreOf(r, 11), scoreOf(r, 10))
	}
	// Only the first film comes back: the score is capped below the threshold, so it is not auto-applied (the host's threshold is 0.9).
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Name: "Zorblax Courier 2", Year: 2003})
	if err != nil || scoreOf(r, 10) >= 0.9 {
		t.Errorf("only the first film came back: score %v, err %v; it must stay below the auto-apply threshold", scoreOf(r, 10), err)
	}
	// Series: a number at the end is the season, and finding the whole series scores high as usual.
	f.results["tv|Zorblax Hanzo 2"] = []map[string]any{{"id": 20, "name": "Zorblax Hanzo", "original_name": "Zorblax Hanzo", "first_air_date": "2013-07-07", "poster_path": "/p.jpg"}}
	r, err = p.Search(ctx, &pluginv1.SearchRequest{Type: pluginv1.ItemType_ITEM_TYPE_SERIES, Name: "Zorblax Hanzo 2", Year: 2013})
	if err != nil || scoreOf(r, 20) < 0.8 {
		t.Errorf("a series with a season number in the query: score %v, err %v", scoreOf(r, 20), err)
	}
}
