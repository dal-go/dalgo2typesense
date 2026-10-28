package dalgo2typesense

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
)

const (
	fixtureImage  = "typesense/typesense:30.2@sha256:610f2d34b1f93d00762869da2c67736775e5798d19a2c8b91b014b8a0cc1e110"
	fixtureEnable = "DALGO2TYPESENSE_FIXTURE"
)

type fixtureEnv struct {
	t         *testing.T
	docker    string
	container string
	volume    string
	endpoint  string
	bootstrap string
	client    *http.Client
}

// TestTypesenseIntegration is opt-in. With DALGO2TYPESENSE_FIXTURE=1 a missing
// Docker daemon, missing image or cleanup failure is a test failure, never a
// skip. Without the variable the test skips and no container is created.
func TestTypesenseIntegration(t *testing.T) {
	if os.Getenv(fixtureEnable) != "1" {
		t.Skip("set " + fixtureEnable + "=1 to run the real Typesense fixture")
	}
	f := startFixture(t)
	f.seed(t)

	searchKey := f.createKey(t, "pure-search", []string{"documents:search"})
	metadataKey := f.createKey(t, "search-and-metadata", []string{
		"documents:search", "collections:get", "collections:list",
	})

	pureDB := f.db(t, "books", searchKey)
	metadataDB := f.db(t, "books", metadataKey)
	pagingDB := f.dbWithConfig(t, "books", searchKey, func(cfg *Config) {
		cfg.MaxRowsPerPage = 3
	})

	t.Run("pure-search-key-works-without-metadata", func(t *testing.T) {
		searcher, _ := dal.As[NativeSearcher](pureDB)
		result, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 10})
		if err != nil {
			t.Fatalf("Search under pure search key: %v", err)
		}
		if result.Found != 12 || result.Returned != 10 || !result.Truncated || result.Complete {
			t.Fatalf("found/returned/truncated = %d/%d/%v/%v", result.Found, result.Returned, result.Truncated, result.Complete)
		}
		reader, _ := dal.As[dbschema.SchemaReader](pureDB)
		if collections, err := reader.ListCollections(context.Background(), nil); !errors.Is(err, ErrMetadataForbidden) {
			t.Fatalf("pure key ListCollections = %v, %v; want ErrMetadataForbidden", collections, err)
		}
	})

	t.Run("metadata-key-discovers-real-schema", func(t *testing.T) {
		reader, _ := dal.As[dbschema.SchemaReader](metadataDB)
		collections, err := reader.ListCollections(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListCollections: %v", err)
		}
		if len(collections) != 1 || collections[0].Name() != "books" {
			t.Fatalf("collections = %+v", collections)
		}
		scopedDB := f.db(t, "", metadataKey)
		scopedReader, _ := dal.As[dbschema.SchemaReader](scopedDB)
		visible, err := scopedReader.ListCollections(context.Background(), nil)
		if err != nil {
			t.Fatalf("scoped ListCollections: %v", err)
		}
		if len(visible) != 1 || visible[0].Name() != "books" {
			t.Fatalf("scoped key exposed unauthorized collections: %+v", visible)
		}
		def, err := reader.DescribeCollection(context.Background(), ptrRef("books"))
		if err != nil {
			t.Fatalf("DescribeCollection: %v", err)
		}
		if def.Name != "books" || len(def.Fields) != 12 {
			t.Fatalf("schema = %+v", def)
		}
		if len(def.PrimaryKey) != 1 || def.PrimaryKey[0] != "id" ||
			def.SourceDefinition == nil || def.SourceDefinition.Dialect != "typesense" {
			t.Fatalf("identity/provenance = %+v", def)
		}
		idField := fieldDefByName(t, def.Fields, "id")
		if idField.Type != dbschema.String || idField.Nullable {
			t.Fatalf("implicit id field = %+v, want non-nullable string", idField)
		}
		idColumn := sourceColumnByName(t, def.SourceDefinition.Columns, "id")
		if idColumn.DeclaredType != "string" || !idColumn.NotNull {
			t.Fatalf("implicit id column = %+v, want declared string and NOT NULL", idColumn)
		}
		if countFieldDefs(def.Fields, "id") != 1 || countSourceColumns(def.SourceDefinition.Columns, "id") != 1 {
			t.Fatalf("id identity is not exactly once: %+v / %+v", def.Fields, def.SourceDefinition.Columns)
		}
		tagsField := fieldDefByName(t, def.Fields, "tags")
		if tagsField.Type != dbschema.Null || tagsField.Type == dbschema.String {
			t.Fatalf("string[] tags portable type = %v, want dbschema.Null and never String", tagsField.Type)
		}
		tagsColumn := sourceColumnByName(t, def.SourceDefinition.Columns, "tags")
		if tagsColumn.DeclaredType != "string[]" {
			t.Fatalf("tags declared type = %q, want string[]", tagsColumn.DeclaredType)
		}
	})

	t.Run("curation-cannot-bypass-filter", func(t *testing.T) {
		unprotected := f.searchIDs(t, searchKey, url.Values{
			"q":         {"*"},
			"filter_by": {"genre:=cyberpunk"},
			"limit":     {"10"},
		})
		if !containsID(unprotected, "001") {
			t.Fatalf("fixture premise failed: default Typesense curations did not promote filtered document 001: %v", unprotected)
		}
		// The raw default query must show the vulnerable premise non-vacuously:
		// the curated 001 plus both real cyberpunk documents.
		assertIDSet(t, unprotected, "001", "004", "006")
		protected := f.searchIDs(t, searchKey, url.Values{
			"q":                   {"*"},
			"filter_by":           {"genre:=cyberpunk"},
			"limit":               {"10"},
			"enable_curations":    {"true"},
			"filter_curated_hits": {"true"},
		})
		if containsID(protected, "001") {
			t.Fatalf("filter_curated_hits=true did not remove curated 001: %v", protected)
		}
		// The protected direct query is also non-vacuous: it still returns both
		// permitted cyberpunk documents.
		assertIDSet(t, protected, "004", "006")

		searcher, _ := dal.As[NativeSearcher](pureDB)
		result, err := searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "*", FilterBy: "genre:=cyberpunk", Limit: 10,
		})
		if err != nil {
			t.Fatalf("adapter search: %v", err)
		}
		ids := hitIDs(result.Hits)
		if containsID(ids, "001") {
			t.Fatalf("adapter let curated 001 bypass filter_by: %v", ids)
		}
		if result.Found != 2 || result.Returned != 2 || len(ids) != 2 {
			t.Fatalf("adapter cyberpunk counts = found %d, returned %d, ids %v; want exactly 004 and 006",
				result.Found, result.Returned, ids)
		}
		assertIDSet(t, ids, "004", "006")

		q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().
			WhereField("genre", dal.Equal, "cyberpunk").Limit(10).SelectIntoRecord(nil)
		reader, err := pureDB.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("structured query: %v", err)
		}
		records, err := dal.ReadAllToRecords(context.Background(), reader)
		if err != nil {
			t.Fatalf("read all: %v", err)
		}
		structuredIDs := make([]string, 0, len(records))
		for _, rec := range records {
			id, ok := rec.Key().ID.(string)
			if !ok {
				t.Fatalf("structured id %#v is not a string", rec.Key().ID)
			}
			structuredIDs = append(structuredIDs, id)
		}
		if containsID(structuredIDs, "001") {
			t.Fatal("structured query let curated 001 bypass filter_by")
		}
		if len(structuredIDs) != 2 {
			t.Fatalf("structured cyberpunk ids = %v; want exactly 004 and 006", structuredIDs)
		}
		assertIDSet(t, structuredIDs, "004", "006")
	})

	t.Run("native-text-query-by-title-and-author", func(t *testing.T) {
		searcher, _ := dal.As[NativeSearcher](pureDB)

		// A unique seeded title with a native filter, explicit sort and numeric
		// facet. Native text search is not SQL: the assertion is only that the
		// real Typesense hit and its native metadata came back.
		result, err := searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "Neuromancer", QueryBy: []string{"title"},
			FilterBy: "genre:=cyberpunk", SortBy: []SortField{{Field: "seq", Descending: true}},
			FacetBy: []string{"year"}, MaxFacetValues: 10, Limit: 5,
		})
		if err != nil {
			t.Fatalf("title search: %v", err)
		}
		if result.Found != 1 || result.Returned != 1 || result.Partial || !result.Complete || result.Truncated {
			t.Fatalf("title search counts = found %d, returned %d, partial %v, complete %v, truncated %v",
				result.Found, result.Returned, result.Partial, result.Complete, result.Truncated)
		}
		if result.Page != 1 || result.Provenance != ProvenanceSearch {
			t.Fatalf("title search page/provenance = %d/%q", result.Page, result.Provenance)
		}
		assertIDSet(t, hitIDs(result.Hits), "004")
		hit := result.Hits[0]
		if hit.Document["id"] != "004" || hit.Document["title"] != "Neuromancer" ||
			hit.Document["author"] != "William Gibson" || hit.Document["genre"] != "cyberpunk" {
			t.Fatalf("title search document identity = %#v", hit.Document)
		}
		if year, ok := hit.Document["year"].(int64); !ok || year != 1984 {
			t.Fatalf("title search year = %#v (%T), want int64 1984", hit.Document["year"], hit.Document["year"])
		}
		if hit.TextMatch <= 0 {
			t.Fatalf("title search text_match = %d, want a positive native rank", hit.TextMatch)
		}
		if hit.Curated {
			t.Fatalf("title search returned a curated hit under enable_curations=false: %#v", hit)
		}
		if len(result.Facets) != 1 || result.Facets[0].Field != "year" {
			t.Fatalf("title search facets = %+v", result.Facets)
		}
		yearFacet := result.Facets[0]
		if len(yearFacet.Counts) != 1 || yearFacet.Counts[0].Value != "1984" || yearFacet.Counts[0].Count != 1 {
			t.Fatalf("title search year facet = %+v", yearFacet.Counts)
		}
		if yearFacet.Stats == nil {
			t.Fatal("title search year facet stats missing")
		}
		if yearFacet.Stats.Min != int64(1984) || yearFacet.Stats.Max != int64(1984) || yearFacet.Stats.Sum != int64(1984) {
			t.Fatalf("title search year facet stats = min %#v, max %#v, sum %#v",
				yearFacet.Stats.Min, yearFacet.Stats.Max, yearFacet.Stats.Sum)
		}

		// An exact seeded author query returns several real documents; the
		// explicit sort proves the returned identity order, and the facets are
		// native Typesense counts/stats rather than SQL aggregates.
		result, err = searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "Frank Herbert", QueryBy: []string{"author"},
			FilterBy: "genre:=science fiction", SortBy: []SortField{{Field: "seq"}},
			FacetBy: []string{"genre", "year"}, MaxFacetValues: 10, Limit: 5,
		})
		if err != nil {
			t.Fatalf("author search: %v", err)
		}
		if result.Found != 3 || result.Returned != 3 || result.Partial || !result.Complete || result.Truncated {
			t.Fatalf("author search counts = found %d, returned %d, partial %v, complete %v, truncated %v",
				result.Found, result.Returned, result.Partial, result.Complete, result.Truncated)
		}
		if got := strings.Join(hitIDs(result.Hits), ","); got != "001,002,003" {
			t.Fatalf("author search ids = %s, want 001,002,003", got)
		}
		for _, hit := range result.Hits {
			if hit.Document["author"] != "Frank Herbert" || hit.TextMatch <= 0 {
				t.Fatalf("author search hit = %#v", hit)
			}
		}
		if len(result.Facets) != 2 {
			t.Fatalf("author search facets = %+v", result.Facets)
		}
		genreFacet := facetForField(t, result.Facets, "genre")
		if len(genreFacet.Counts) != 1 || genreFacet.Counts[0].Value != "science fiction" || genreFacet.Counts[0].Count != 3 {
			t.Fatalf("author search genre facet = %+v", genreFacet.Counts)
		}
		authorYearFacet := facetForField(t, result.Facets, "year")
		if len(authorYearFacet.Counts) != 3 {
			t.Fatalf("author search year facet = %+v", authorYearFacet.Counts)
		}
		wantYears := map[string]int{"1965": 1, "1969": 1, "1976": 1}
		for _, count := range authorYearFacet.Counts {
			if wantYears[count.Value] != count.Count {
				t.Fatalf("author search year facet = %+v, want %v", authorYearFacet.Counts, wantYears)
			}
		}
		if authorYearFacet.Stats == nil {
			t.Fatal("author search year facet stats missing")
		}
		if authorYearFacet.Stats.Min != int64(1965) || authorYearFacet.Stats.Max != int64(1976) ||
			authorYearFacet.Stats.Sum != int64(5910) {
			t.Fatalf("author search year facet stats = min %#v, max %#v, sum %#v",
				authorYearFacet.Stats.Min, authorYearFacet.Stats.Max, authorYearFacet.Stats.Sum)
		}

		// Native Search deliberately does not send num_typos/prefix/
		// drop_tokens_threshold, so these assertions pin the real 30.2 default
		// semantics: one typo is tolerated, a partial final token matches by
		// prefix, and a token with no hits is dropped. Structured DALgo queries
		// are the ones that disable these search behaviours.
		prefix, err := searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "Neuromanc", QueryBy: []string{"title"}, Limit: 5,
		})
		if err != nil {
			t.Fatalf("prefix search: %v", err)
		}
		if prefix.Found != 1 || prefix.Returned != 1 {
			t.Fatalf("prefix search counts = found %d, returned %d", prefix.Found, prefix.Returned)
		}
		assertIDSet(t, hitIDs(prefix.Hits), "004")

		// One missing character is within the native default typo tolerance.
		typo, err := searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "Neuromancr", QueryBy: []string{"title"}, Limit: 5,
		})
		if err != nil {
			t.Fatalf("typo search: %v", err)
		}
		if typo.Found != 1 || typo.Returned != 1 {
			t.Fatalf("typo search counts = found %d, returned %d", typo.Found, typo.Returned)
		}
		assertIDSet(t, hitIDs(typo.Hits), "004")

		dropped, err := searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "Dune Herbert", QueryBy: []string{"title"}, Limit: 5,
		})
		if err != nil {
			t.Fatalf("drop-token search: %v", err)
		}
		if dropped.Found != 3 || dropped.Returned != 3 {
			t.Fatalf("drop-token search counts = found %d, returned %d", dropped.Found, dropped.Returned)
		}
		assertIDSet(t, hitIDs(dropped.Hits), "001", "002", "003")
	})

	t.Run("native-fidelity-and-paging", func(t *testing.T) {
		searcher, _ := dal.As[NativeSearcher](pureDB)
		result, err := searcher.Search(context.Background(), SearchRequest{
			Collection: "books", Q: "*", FilterBy: "genre:=science fiction",
			SortBy: []SortField{{Field: "seq"}}, FacetBy: []string{"genre"}, MaxFacetValues: 10, Limit: 10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(result.Facets) != 1 || result.Facets[0].Field != "genre" {
			t.Fatalf("facets = %+v", result.Facets)
		}
		if result.Facets[0].Stats == nil {
			t.Fatal("native facet stats missing")
		}
		foundDune := false
		for _, hit := range result.Hits {
			if hit.ID == "001" {
				foundDune = true
				if hit.Document["id"] != "001" {
					t.Fatal("string id changed")
				}
				if value, ok := hit.Document["big"].(int64); !ok || value != 9007199254740993 {
					t.Fatalf("big int64 = %#v (%T)", hit.Document["big"], hit.Document["big"])
				}
				if value, ok := hit.Document["ratio"].(float64); !ok || value != 1.5 {
					t.Fatalf("float = %#v (%T)", hit.Document["ratio"], hit.Document["ratio"])
				}
				if _, ok := hit.Document["tags"].([]any); !ok {
					t.Fatalf("tags array = %T", hit.Document["tags"])
				}
				if hit.Document["none"] != nil {
					t.Fatal("unexpected field")
				}
			}
		}
		if !foundDune {
			t.Fatal("native search did not return document 001")
		}

		q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().
			OrderBy(dal.AscendingField("seq")).Limit(7).SelectIntoRecord(nil)
		reader, err := pagingDB.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("paged reader: %v", err)
		}
		var ids []string
		for {
			rec, err := reader.Next()
			if errors.Is(err, dal.ErrNoMoreRecords) {
				break
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			ids = append(ids, rec.Key().ID.(string))
		}
		_ = reader.Close()
		if strings.Join(ids, ",") != "001,002,003,004,005,006,007" {
			t.Fatalf("paged ids = %v", ids)
		}
	})

	t.Run("native-default-sort-ties-and-maxsort", func(t *testing.T) {
		searcher, _ := dal.As[NativeSearcher](pureDB)
		defaultResult, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 12})
		if err != nil {
			t.Fatalf("default-sort search: %v", err)
		}
		if defaultResult.Returned != 12 || !defaultResult.Complete {
			t.Fatalf("default-sort returned/complete = %d/%v", defaultResult.Returned, defaultResult.Complete)
		}
		ranks := documentRanks(t, defaultResult.Hits)
		wantRanks := []int64{100, 90, 80, 70, 60, 50, 40, 30, 20, 20, 10, 10}
		if !equalInt64s(ranks, wantRanks) {
			t.Fatalf("default-sort ranks = %v, want %v", ranks, wantRanks)
		}
		if defaultResult.Hits[0].ID != "012" {
			t.Fatalf("default-sort first id = %q, want 012", defaultResult.Hits[0].ID)
		}
		tiedRanks := map[int64][]string{}
		for _, hit := range defaultResult.Hits {
			rank := hit.Document["rank"].(int64)
			tiedRanks[rank] = append(tiedRanks[rank], hit.ID)
		}
		assertIDSet(t, tiedRanks[20], "003", "004")
		assertIDSet(t, tiedRanks[10], "001", "002")

		// rank has duplicate values. An explicit second sort field makes the
		// page boundary repeatable without inventing an id sort.
		twoSorts := []SortField{{Field: "rank", Descending: true}, {Field: "seq"}}
		tied, err := searcher.Search(context.Background(), SearchRequest{
			Q: "*", SortBy: twoSorts, Limit: 12,
		})
		if err != nil {
			t.Fatalf("two-field sort: %v", err)
		}
		wantIDs := []string{"012", "011", "010", "009", "008", "007", "006", "005", "003", "004", "001", "002"}
		if got := strings.Join(hitIDs(tied.Hits), ","); got != strings.Join(wantIDs, ",") {
			t.Fatalf("two-field sort ids = %s, want %s", got, strings.Join(wantIDs, ","))
		}

		// The server accepts three sort fields; the adapter accepts exactly the
		// same maximum and refuses a fourth before any request.
		threeSorts := append(append([]SortField(nil), twoSorts...), SortField{Field: "year"})
		three, err := searcher.Search(context.Background(), SearchRequest{
			Q: "*", SortBy: threeSorts, Limit: 12,
		})
		if err != nil {
			t.Fatalf("three-field sort: %v", err)
		}
		if got := strings.Join(hitIDs(three.Hits), ","); got != strings.Join(wantIDs, ",") {
			t.Fatalf("three-field sort ids = %s, want %s", got, strings.Join(wantIDs, ","))
		}
		fourSorts := append(append([]SortField(nil), threeSorts...), SortField{Field: "active"})
		if _, err = searcher.Search(context.Background(), SearchRequest{Q: "*", SortBy: fourSorts, Limit: 1}); !errors.Is(err, dal.ErrNotSupported) {
			t.Fatalf("four-field sort err = %v, want ErrNotSupported", err)
		}
		if _, err = searcher.Search(context.Background(), SearchRequest{
			Q: "*", SortBy: []SortField{{Field: "id"}}, Limit: 1,
		}); !errors.Is(err, ErrUnsafeValue) {
			t.Fatalf("id sort err = %v, want ErrUnsafeValue", err)
		}

		// A structured query with no explicit order uses the server's real
		// default_sorting_field, not a fabricated id order.
		q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().
			Limit(12).SelectIntoRecord(nil)
		reader, err := pureDB.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("default-sort reader: %v", err)
		}
		var defaultIDs []string
		var defaultRanks []int64
		for {
			rec, nextErr := reader.Next()
			if errors.Is(nextErr, dal.ErrNoMoreRecords) {
				break
			}
			if nextErr != nil {
				t.Fatalf("default-sort Next: %v", nextErr)
			}
			defaultIDs = append(defaultIDs, rec.Key().ID.(string))
			defaultRanks = append(defaultRanks, rec.Data().(map[string]any)["rank"].(int64))
		}
		_ = reader.Close()
		if len(defaultIDs) != 12 || defaultIDs[0] != "012" {
			t.Fatalf("structured default-sort ids = %v", defaultIDs)
		}
		if !equalInt64s(defaultRanks, wantRanks) {
			t.Fatalf("structured default-sort ranks = %v, want %v", defaultRanks, wantRanks)
		}
	})

	t.Run("native-offset-at-or-beyond-found", func(t *testing.T) {
		searcher, _ := dal.As[NativeSearcher](pagingDB)
		for _, tc := range []struct {
			name   string
			offset int
		}{
			{"at-found", 12},
			{"beyond-found", 15},
		} {
			result, err := searcher.Search(context.Background(), SearchRequest{
				Q: "*", Limit: 3, Offset: tc.offset, SortBy: []SortField{{Field: "seq"}},
			})
			if err != nil {
				t.Fatalf("%s: Search: %v", tc.name, err)
			}
			if result.Returned != 0 || result.Found != 12 {
				t.Fatalf("%s: returned/found = %d/%d, want 0/12",
					tc.name, result.Returned, result.Found)
			}
			if result.Partial || !result.Complete {
				t.Fatalf("%s: partial/complete = %v/%v, want false/true",
					tc.name, result.Partial, result.Complete)
			}
		}

		q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().
			OrderBy(dal.AscendingField("seq")).Limit(5).Offset(10).SelectIntoRecord(nil)
		reader, err := pagingDB.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("offset reader: %v", err)
		}
		var ids []string
		for {
			rec, nextErr := reader.Next()
			if errors.Is(nextErr, dal.ErrNoMoreRecords) {
				break
			}
			if nextErr != nil {
				t.Fatalf("offset Next: %v", nextErr)
			}
			ids = append(ids, rec.Key().ID.(string))
		}
		_ = reader.Close()
		if strings.Join(ids, ",") != "011,012" {
			t.Fatalf("offset ids = %v, want [011 012]", ids)
		}
	})

	t.Run("configured-budgets-are-enforced", func(t *testing.T) {
		pageDB := f.dbWithConfig(t, "books", searchKey, func(cfg *Config) {
			cfg.MaxRowsPerPage = 2
		})
		pageSearcher, _ := dal.As[NativeSearcher](pageDB)
		if _, err := pageSearcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 3}); !errors.Is(err, dal.ErrNotSupported) {
			t.Fatalf("oversized native search err = %v, want ErrNotSupported", err)
		}

		rowDB := f.dbWithConfig(t, "books", searchKey, func(cfg *Config) {
			cfg.MaxRowBytes = 16
		})
		rec := record.NewRecordWithData(record.NewKeyWithID("books", "001"), &map[string]any{})
		if err := rowDB.Get(context.Background(), rec); !errors.Is(err, ErrRowTooLarge) {
			t.Fatalf("over-budget Get err = %v, want ErrRowTooLarge", err)
		}
	})

	t.Run("projection-null-and-escaping", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().
			WhereField("note", dal.Equal, "a b (c) && d").
			Limit(1).
			SelectColumns(
				dal.Column{Expression: dal.Field("title")},
				dal.Column{Expression: dal.Field("big")},
				dal.Column{Expression: dal.Field("seq")},
			)
		reader, err := pureDB.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		rec, err := reader.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if rec.Key().ID != "003" {
			t.Fatalf("id = %v", rec.Key().ID)
		}
		data := rec.Data().(map[string]any)
		if len(data) != 3 {
			t.Fatalf("projected width = %d: %#v", len(data), data)
		}
		if _, leaked := data["id"]; leaked {
			t.Fatal("helper id leaked")
		}
		if data["big"].(int64) != 9223372036854775807 {
			t.Fatalf("max int64 = %#v", data["big"])
		}
		_ = reader.Close()

		q = dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().
			WhereField("id", dal.Equal, "001").Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("title")})
		reader, err = pureDB.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("id reader: %v", err)
		}
		rec, err = reader.Next()
		if err != nil || rec.Key().ID != "001" {
			t.Fatalf("id filter: %v, %v", rec, err)
		}
		_ = reader.Close()
	})

	t.Run("least-privilege-and-writes", func(t *testing.T) {
		// The search key cannot read an unauthorized collection.
		otherDB := f.db(t, "", searchKey)
		otherSearcher, _ := dal.As[NativeSearcher](otherDB)
		_, err := otherSearcher.Search(context.Background(), SearchRequest{Collection: "archive", Q: "*", Limit: 1})
		if !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("unauthorized collection err = %v", err)
		}

		// Bad key is classified, never treated as an empty result.
		badDB := f.db(t, "books", "definitely-not-a-key")
		badSearcher, _ := dal.As[NativeSearcher](badDB)
		if _, err = badSearcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("bad key err = %v", err)
		}

		// Both fixture reader keys are read-only.
		for _, key := range []string{searchKey, metadataKey} {
			status, body := f.do(t, key, http.MethodPost, "/collections/books/documents", `{"id":"write-probe"}`)
			if status != http.StatusUnauthorized && status != http.StatusForbidden {
				t.Fatalf("write with reader key = HTTP %d, %s", status, body)
			}
		}

		// Exact-id reads use the same search-only surface.
		rec := record.NewRecordWithData(record.NewKeyWithID("books", "001"), &map[string]any{})
		if err = pureDB.Get(context.Background(), rec); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if (*rec.Data().(*map[string]any))["title"] != "Dune" {
			t.Fatalf("Get data = %#v", rec.Data())
		}
		if exists, err := pureDB.Exists(context.Background(), record.NewKeyWithID("books", "002")); err != nil || !exists {
			t.Fatalf("Exists = %v, %v", exists, err)
		}
		batch := []record.Record{
			record.NewRecordWithData(record.NewKeyWithID("books", "001"), &map[string]any{}),
			record.NewRecordWithData(record.NewKeyWithID("books", "003"), &map[string]any{}),
		}
		if err = pureDB.GetMulti(context.Background(), batch); err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		if (*batch[1].Data().(*map[string]any))["title"] != "Children of Dune" {
			t.Fatalf("GetMulti data = %#v", batch[1].Data())
		}
	})
}

func startFixture(t *testing.T) *fixtureEnv {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("Docker is required when %s=1", fixtureEnable)
	}
	name := "dalgo2typesense-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	volume := name + "-data"
	f := &fixtureEnv{
		t:         t,
		docker:    docker,
		container: name,
		volume:    volume,
		bootstrap: "bootstrap-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		client:    &http.Client{Timeout: 5 * time.Second},
	}
	if output, err := f.run("volume", "create", volume); err != nil {
		t.Fatalf("docker volume create: %v: %s", err, output)
	}
	containerCreated := false
	t.Cleanup(func() {
		cleanupErr := false
		if containerCreated {
			if output, err := f.run("rm", "-f", name); err != nil {
				t.Errorf("fixture cleanup: docker rm -f: %v: %s", err, output)
				cleanupErr = true
			}
		}
		if output, err := f.run("volume", "rm", "-f", volume); err != nil {
			t.Errorf("fixture cleanup: docker volume rm: %v: %s", err, output)
			cleanupErr = true
		}
		if _, err := f.run("inspect", name); err == nil {
			t.Errorf("fixture cleanup: container %s still exists", name)
			cleanupErr = true
		}
		if _, err := f.run("volume", "inspect", volume); err == nil {
			t.Errorf("fixture cleanup: volume %s still exists", volume)
			cleanupErr = true
		}
		if !cleanupErr {
			t.Logf("fixture cleanup verified: container %s and volume %s removed", name, volume)
		}
	})

	runArgs := []string{
		"run", "-d", "--name", name,
		"--label", "dalgo2typesense.fixture=1",
		"-p", "127.0.0.1::8108",
		"-v", volume + ":/data",
		fixtureImage,
		"--data-dir=/data", "--api-key=" + f.bootstrap, "--listen-port=8108", "--enable-cors",
	}
	if output, err := f.run(runArgs...); err != nil {
		t.Fatalf("docker run: %v: %s", err, output)
	}
	containerCreated = true
	portOutput, err := f.run("port", name, "8108")
	if err != nil {
		t.Fatalf("docker port: %v: %s", err, portOutput)
	}
	parts := strings.Split(strings.TrimSpace(portOutput), ":")
	port := strings.TrimSpace(parts[len(parts)-1])
	if port == "" {
		t.Fatalf("docker port output = %q", portOutput)
	}
	f.endpoint = "http://127.0.0.1:" + port
	f.waitHealthy()
	status, body := f.do(t, f.bootstrap, http.MethodGet, "/debug", "")
	if status != http.StatusOK || !strings.Contains(body, `"version":"30.2`) {
		t.Fatalf("fixture version assertion failed: HTTP %d, %s", status, body)
	}
	return f
}

func (f *fixtureEnv) run(args ...string) (string, error) {
	output, err := exec.Command(f.docker, args...).CombinedOutput()
	return string(output), err
}

func (f *fixtureEnv) waitHealthy() {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, body, err := f.try(f.bootstrap, http.MethodGet, "/health", "")
		if err == nil && status == http.StatusOK && strings.Contains(body, `"ok":true`) {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("HTTP %d, %s", status, body)
		}
		time.Sleep(250 * time.Millisecond)
	}
	f.t.Fatalf("fixture did not become healthy: %s", last)
}

func (f *fixtureEnv) try(key, method, path, body string) (int, string, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.endpoint+path, reader)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("X-TYPESENSE-API-KEY", key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(payload), nil
}

func (f *fixtureEnv) do(t *testing.T, key, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.endpoint+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-TYPESENSE-API-KEY", key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("fixture request %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("fixture read body: %v", err)
	}
	return resp.StatusCode, string(payload)
}

func (f *fixtureEnv) seed(t *testing.T) {
	t.Helper()
	schema := `{
		"name":"books",
		"fields":[
			{"name":"title","type":"string"},
			{"name":"author","type":"string","facet":true},
			{"name":"genre","type":"string","facet":true},
			{"name":"year","type":"int32","facet":true},
			{"name":"rank","type":"int64"},
			{"name":"seq","type":"int64","sort":true},
			{"name":"big","type":"int64"},
			{"name":"ratio","type":"float"},
			{"name":"active","type":"bool","facet":true},
			{"name":"tags","type":"string[]","facet":true},
			{"name":"note","type":"string","optional":true}
		],
		"default_sorting_field":"rank"
	}`
	if status, body := f.do(t, f.bootstrap, http.MethodPost, "/collections", schema); status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create collection: HTTP %d, %s", status, body)
	}
	archiveSchema := `{"name":"archive","fields":[{"name":"title","type":"string"}]}`
	if status, body := f.do(t, f.bootstrap, http.MethodPost, "/collections", archiveSchema); status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create archive collection: HTTP %d, %s", status, body)
	}
	if status, body := f.do(t, f.bootstrap, http.MethodPost,
		"/collections/archive/documents/import?action=upsert", `{"id":"a1","title":"Archive"}`); status != http.StatusOK || strings.Contains(body, `"success":false`) {
		t.Fatalf("import archive: HTTP %d, %s", status, body)
	}
	documents := []string{
		`{"id":"001","title":"Dune","author":"Frank Herbert","genre":"science fiction","year":1965,"rank":10,"seq":1,"big":9007199254740993,"ratio":1.5,"active":true,"tags":["classic","desert"]}`,
		`{"id":"002","title":"Dune Messiah","author":"Frank Herbert","genre":"science fiction","year":1969,"rank":10,"seq":2,"big":-9223372036854775808,"ratio":2.25,"active":false,"tags":["sequel"]}`,
		`{"id":"003","title":"Children of Dune","author":"Frank Herbert","genre":"science fiction","year":1976,"rank":20,"seq":3,"big":9223372036854775807,"ratio":3.5,"active":true,"tags":["sequel"],"note":"a b (c) && d"}`,
		`{"id":"004","title":"Neuromancer","author":"William Gibson","genre":"cyberpunk","year":1984,"rank":20,"seq":4,"big":0,"ratio":4.5,"active":true,"tags":["classic"]}`,
		`{"id":"005","title":"Foundation","author":"Isaac Asimov","genre":"science fiction","year":1951,"rank":30,"seq":5,"big":42,"ratio":1.0,"active":false,"tags":["classic"],"note":null}`,
		`{"id":"006","title":"Snow Crash","author":"Neal Stephenson","genre":"cyberpunk","year":1992,"rank":40,"seq":6,"big":84,"ratio":2.0,"active":true,"tags":["classic"]}`,
		`{"id":"007","title":"The Left Hand of Darkness","author":"Ursula K. Le Guin","genre":"science fiction","year":1969,"rank":50,"seq":7,"big":7,"ratio":0.5,"active":true,"tags":["classic"]}`,
		`{"id":"008","title":"Hyperion","author":"Dan Simmons","genre":"science fiction","year":1989,"rank":60,"seq":8,"big":8,"ratio":0.25,"active":true,"tags":["classic"]}`,
		`{"id":"009","title":"The Dispossessed","author":"Ursula K. Le Guin","genre":"science fiction","year":1974,"rank":70,"seq":9,"big":9,"ratio":0.125,"active":true,"tags":["classic"]}`,
		`{"id":"010","title":"Ender's Game","author":"Orson Scott Card","genre":"science fiction","year":1985,"rank":80,"seq":10,"big":10,"ratio":0.0625,"active":true,"tags":["classic"]}`,
		`{"id":"011","title":"The Stars My Destination","author":"Alfred Bester","genre":"science fiction","year":1956,"rank":90,"seq":11,"big":11,"ratio":0.03125,"active":true,"tags":["classic"]}`,
		`{"id":"012","title":"A Canticle for Leibowitz","author":"Walter M. Miller Jr.","genre":"science fiction","year":1959,"rank":100,"seq":12,"big":12,"ratio":0.015625,"active":true,"tags":["classic"]}`,
	}
	status, body := f.do(t, f.bootstrap, http.MethodPost,
		"/collections/books/documents/import?action=upsert", strings.Join(documents, "\n"))
	if status != http.StatusOK || strings.Contains(body, `"success":false`) {
		t.Fatalf("import documents: HTTP %d, %s", status, body)
	}

	curation := `{"items":[{
		"id":"pin-scifi-doc",
		"rule":{"filter_by":"genre:=cyberpunk"},
		"includes":[{"id":"001","position":1}]
	}]}`
	if status, body = f.do(t, f.bootstrap, http.MethodPut, "/curation_sets/books_curations", curation); status != http.StatusOK {
		t.Fatalf("create curation: HTTP %d, %s", status, body)
	}
	if status, body = f.do(t, f.bootstrap, http.MethodPatch, "/collections/books", `{"curation_sets":["books_curations"]}`); status != http.StatusOK {
		t.Fatalf("link curation: HTTP %d, %s", status, body)
	}
}

func (f *fixtureEnv) createKey(t *testing.T, description string, actions []string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"description": description,
		"actions":     actions,
		"collections": []string{"books"},
	})
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	status, response := f.do(t, f.bootstrap, http.MethodPost, "/keys", string(body))
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create key %s: HTTP %d, %s", description, status, response)
	}
	var decoded struct {
		Value string `json:"value"`
	}
	if err = json.Unmarshal([]byte(response), &decoded); err != nil || decoded.Value == "" {
		t.Fatalf("create key %s: decode: %v, %s", description, err, response)
	}
	return decoded.Value
}

func (f *fixtureEnv) db(t *testing.T, collection, key string) dal.DB {
	t.Helper()
	return f.dbWithConfig(t, collection, key, nil)
}

func (f *fixtureEnv) dbWithConfig(t *testing.T, collection, key string, mutate func(*Config)) dal.DB {
	t.Helper()
	cfg := Config{
		Endpoint:          f.endpoint,
		Collection:        collection,
		AllowLoopbackHTTP: true,
		Headers: func(context.Context) (map[string]string, error) {
			return map[string]string{"X-TYPESENSE-API-KEY": key}, nil
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	db, err := NewDB(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := dal.As[io.Closer](db); ok {
			_ = closer.Close()
		}
	})
	return db
}

func (f *fixtureEnv) searchIDs(t *testing.T, key string, params url.Values) []string {
	t.Helper()
	status, body := f.do(t, key, http.MethodGet, "/collections/books/documents/search?"+params.Encode(), "")
	if status != http.StatusOK {
		t.Fatalf("raw search: HTTP %d, %s", status, body)
	}
	var decoded struct {
		Hits []struct {
			Document struct {
				ID string `json:"id"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&decoded); err != nil {
		t.Fatalf("raw search decode: %v", err)
	}
	ids := make([]string, 0, len(decoded.Hits))
	for _, hit := range decoded.Hits {
		ids = append(ids, hit.Document.ID)
	}
	return ids
}

func hitIDs(hits []SearchHit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func facetForField(t *testing.T, facets []Facet, field string) Facet {
	t.Helper()
	for _, facet := range facets {
		if facet.Field == field {
			return facet
		}
	}
	t.Fatalf("facet %q missing from %+v", field, facets)
	return Facet{}
}

func documentRanks(t *testing.T, hits []SearchHit) []int64 {
	t.Helper()
	ranks := make([]int64, len(hits))
	for index, hit := range hits {
		rank, ok := hit.Document["rank"].(int64)
		if !ok {
			t.Fatalf("hit %q rank = %#v (%T), want int64", hit.ID, hit.Document["rank"], hit.Document["rank"])
		}
		ranks[index] = rank
	}
	return ranks
}

func equalInt64s(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func assertIDSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
