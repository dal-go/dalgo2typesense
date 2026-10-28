package dalgo2typesense

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// TestSearch_OffsetCountConsistency keeps the native surface as strict as the
// DALgo reader: a non-empty page whose found count cannot cover the requested
// offset plus its hits is malformed, while a valid empty page at or beyond
// found stays a complete result.
func TestSearch_OffsetCountConsistency(t *testing.T) {
	t.Run("nonempty-contradiction", func(t *testing.T) {
		body := `{"found":1,"out_of":9,"page":1,"hits":[{"document":{"id":"a"}}]}`
		_, err := runNativeSearch(t, body, SearchRequest{Q: "*", Limit: 5, Offset: 10})
		if !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("err = %v, want ErrMalformedResponse", err)
		}
	})
	t.Run("empty-at-found-is-complete", func(t *testing.T) {
		body := `{"found":3,"out_of":9,"page":1,"hits":[]}`
		result, err := runNativeSearch(t, body, SearchRequest{Q: "*", Limit: 5, Offset: 3})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if result.Returned != 0 || result.Found != 3 || result.Partial || !result.Complete {
			t.Fatalf("result = %+v, want empty complete page", result)
		}
	})
	t.Run("empty-beyond-found-is-complete", func(t *testing.T) {
		body := `{"found":3,"out_of":9,"page":1,"hits":[]}`
		result, err := runNativeSearch(t, body, SearchRequest{Q: "*", Limit: 5, Offset: 10})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if result.Returned != 0 || result.Found != 3 || result.Partial || !result.Complete {
			t.Fatalf("result = %+v, want empty complete page", result)
		}
	})
	t.Run("exact-count-fill", func(t *testing.T) {
		body := `{"found":3,"out_of":9,"page":1,"hits":[{"document":{"id":"c"}}]}`
		result, err := runNativeSearch(t, body, SearchRequest{Q: "*", Limit: 5, Offset: 2})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if result.Returned != 1 || result.Found != 3 || result.Partial || !result.Complete {
			t.Fatalf("result = %+v, want complete final hit", result)
		}
	})
	t.Run("cutoff-offset-stays-partial", func(t *testing.T) {
		body := `{"found":0,"out_of":9,"page":1,"hits":[],"search_cutoff":true}`
		result, err := runNativeSearch(t, body, SearchRequest{Q: "*", Limit: 5, Offset: 10})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if !result.Partial || result.Complete {
			t.Fatalf("result = %+v, want partial cutoff", result)
		}
	})
}

// TestSearch_FacetResponseValidatedAgainstRequest proves the response is checked
// against the exact bounded facet request and effective value budget before any
// facet is exposed. Typesense 30.2 returns every requested facet, so a missing
// one is treated as a truncated/malformed response rather than silently ignored.
func TestSearch_FacetResponseValidatedAgainstRequest(t *testing.T) {
	t.Run("valid-exact-request", func(t *testing.T) {
		body := `{"found":0,"out_of":0,"page":1,"hits":[],"facet_counts":[
			{"field_name":"genre","counts":[{"value":"scifi","count":2,"highlighted":"scifi"}],
			 "stats":{"total_values":1}},
			{"field_name":"year","counts":[{"value":"1984","count":1}],
			 "stats":{"min":1984,"max":1984,"sum":1984,"avg":1984,"total_values":1}}]}`
		result, err := runNativeSearch(t, body, SearchRequest{
			Q: "*", Limit: 1, FacetBy: []string{"genre", "year"}, MaxFacetValues: 2,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(result.Facets) != 2 {
			t.Fatalf("facets = %+v, want the two requested fields", result.Facets)
		}
		genre := facetForField(t, result.Facets, "genre")
		if len(genre.Counts) != 1 || genre.Counts[0].Value != "scifi" || genre.Counts[0].Count != 2 {
			t.Fatalf("genre facet = %+v", genre)
		}
		year := facetForField(t, result.Facets, "year")
		if year.Stats == nil || year.Stats.Min != int64(1984) || year.Stats.TotalValues != 1 {
			t.Fatalf("year facet stats = %+v", year.Stats)
		}
	})

	for _, tc := range []struct {
		name    string
		request SearchRequest
		facets  string
	}{
		{
			name:    "unrequested-field",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets:  `[{"field_name":"author","counts":[]}]`,
		},
		{
			name:    "missing-requested-field",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre", "year"}, MaxFacetValues: 2},
			facets:  `[{"field_name":"genre","counts":[]}]`,
		},
		{
			name:    "duplicate-field",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre", "year"}, MaxFacetValues: 2},
			facets: `[{"field_name":"genre","counts":[]},
				{"field_name":"genre","counts":[]}]`,
		},
		{
			name:    "over-value-budget",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 1},
			facets: `[{"field_name":"genre","counts":[
				{"value":"a","count":1},{"value":"b","count":1}]}]`,
		},
		{
			name:    "duplicate-value",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets: `[{"field_name":"genre","counts":[
				{"value":"a","count":1},{"value":"a","count":1}]}]`,
		},
		{
			name:    "non-string-highlighted",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets:  `[{"field_name":"genre","counts":[{"value":"a","count":1,"highlighted":7}]}]`,
		},
		{
			name:    "negative-count",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets:  `[{"field_name":"genre","counts":[{"value":"a","count":-1}]}]`,
		},
		{
			name:    "missing-value",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets:  `[{"field_name":"genre","counts":[{"count":1}]}]`,
		},
		{
			name:    "missing-counts",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets:  `[{"field_name":"genre"}]`,
		},
		{
			name:    "missing-requested-array",
			request: SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 2},
			facets:  ``,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"found":0,"out_of":0,"page":1,"hits":[]`
			if tc.facets != "" {
				body += `,"facet_counts":` + tc.facets
			}
			body += `}`
			_, err := runNativeSearch(t, body, tc.request)
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

// TestSearch_DefaultFacetBudgetRespectsConfig proves MaxFacetValues=0 uses the
// documented default 10 but is still capped by the configured bound, and that
// the same effective bound validates the response.
func TestSearch_DefaultFacetBudgetRespectsConfig(t *testing.T) {
	t.Run("configured-cap", func(t *testing.T) {
		var requestedBound string
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			requestedBound = r.URL.Query().Get("max_facet_values")
			writeJSON(t, w, http.StatusOK, facetBody(2))
		})
		db := newTestDB(t, server, func(cfg *Config) { cfg.MaxFacetValues = 1 })
		searcher, _ := dal.As[NativeSearcher](db)
		_, err := searcher.Search(context.Background(), SearchRequest{
			Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 0,
		})
		if !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("err = %v, want ErrMalformedResponse for two values under a configured cap of one", err)
		}
		if requestedBound != "1" {
			t.Fatalf("max_facet_values = %q, want configured cap 1", requestedBound)
		}
	})
	t.Run("default-ten", func(t *testing.T) {
		var requestedBound string
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			requestedBound = r.URL.Query().Get("max_facet_values")
			writeJSON(t, w, http.StatusOK, facetBody(11))
		})
		db := newTestDB(t, server, nil)
		searcher, _ := dal.As[NativeSearcher](db)
		_, err := searcher.Search(context.Background(), SearchRequest{
			Q: "*", Limit: 1, FacetBy: []string{"genre"}, MaxFacetValues: 0,
		})
		if !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("err = %v, want ErrMalformedResponse for eleven values under the default ten", err)
		}
		if requestedBound != "10" {
			t.Fatalf("max_facet_values = %q, want default 10", requestedBound)
		}
	})
}

func facetBody(values int) string {
	var counts strings.Builder
	for i := 0; i < values; i++ {
		if i > 0 {
			counts.WriteString(",")
		}
		fmt.Fprintf(&counts, `{"value":"v%d","count":1}`, i)
	}
	return `{"found":0,"out_of":0,"page":1,"hits":[],"facet_counts":[
		{"field_name":"genre","counts":[` + counts.String() + `]}]}`
}

// TestStructuredRead_NoFacetsIsStrict proves the structured read paths never
// expose facet data they did not request, while an empty facet_counts array
// remains accepted.
func TestStructuredRead_NoFacetsIsStrict(t *testing.T) {
	t.Run("unexpected-facets-refused", func(t *testing.T) {
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"a"}}],
				"facet_counts":[{"field_name":"genre","counts":[]}]}`)
		})
		db := newTestDB(t, server, nil)
		query := dal.From(dal.NewRootCollectionRef("books", "")).
			NewQuery().Limit(1).SelectIntoRecord(nil)
		reader, err := readerForQuery(t, db, query)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		if _, err = reader.Next(); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("Next err = %v, want ErrMalformedResponse", err)
		}
	})
	t.Run("empty-facets-accepted", func(t *testing.T) {
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"a"}}],"facet_counts":[]}`)
		})
		db := newTestDB(t, server, nil)
		query := dal.From(dal.NewRootCollectionRef("books", "")).
			NewQuery().Limit(1).SelectIntoRecord(nil)
		reader, err := readerForQuery(t, db, query)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		rec, err := reader.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if rec.Key().ID.(string) != "a" {
			t.Fatalf("id = %v", rec.Key().ID)
		}
	})
}
