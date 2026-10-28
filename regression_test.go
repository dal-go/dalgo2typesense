package dalgo2typesense

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func readerForQuery(t *testing.T, db dal.DB, query dal.Query) (dal.RecordsReader, error) {
	t.Helper()
	return db.ExecuteQueryToRecordsReader(context.Background(), query)
}

// TestReader_OffsetAtOrBeyondFoundIsEOF covers a valid empty page: when the
// requested offset is at or beyond the server's found count there are no hits
// and the reader must report a clean end of records, not a malformed response.
func TestReader_OffsetAtOrBeyondFoundIsEOF(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		if offset != "12" && offset != "20" {
			t.Errorf("unexpected offset %q", offset)
		}
		writeJSON(t, w, http.StatusOK, `{"found":12,"out_of":12,"page":1,"hits":[]}`)
	})
	db := newTestDB(t, server, nil)
	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"at-found", 12},
		{"beyond-found", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := dal.From(dal.NewRootCollectionRef("books", "")).
				NewQuery().Limit(3).Offset(tc.offset).SelectIntoRecord(nil)
			reader, err := readerForQuery(t, db, query)
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			defer func() { _ = reader.Close() }()
			if _, err = reader.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
				t.Fatalf("Next err = %v, want ErrNoMoreRecords", err)
			}
			if errors.Is(err, ErrPartialResult) {
				t.Fatal("empty page at or beyond found was reported as partial")
			}
			if _, err = reader.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
				t.Fatalf("second Next err = %v, want ErrNoMoreRecords", err)
			}
		})
	}
}

// TestReader_MalformedNonEmptyCounts keeps the contradiction check strict: a
// non-empty page whose found count cannot cover the requested offset and the
// returned hits is malformed, never EOF.
func TestReader_MalformedNonEmptyCounts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int
		body   string
	}{
		{
			name:   "found-below-offset-plus-hit",
			offset: 5,
			body:   `{"found":4,"out_of":9,"page":1,"hits":[{"document":{"id":"a","title":"A"}}]}`,
		},
		{
			name:   "found-below-returned-hits",
			offset: 0,
			body:   `{"found":0,"out_of":9,"page":1,"hits":[{"document":{"id":"a","title":"A"}}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, tc.body)
			})
			db := newTestDB(t, server, nil)
			query := dal.From(dal.NewRootCollectionRef("books", "")).
				NewQuery().Limit(3).Offset(tc.offset).SelectIntoRecord(nil)
			reader, err := readerForQuery(t, db, query)
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			defer func() { _ = reader.Close() }()
			if _, err = reader.Next(); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("Next err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

// TestReader_MultiPageFinalShortPage proves a short page that exactly reaches
// the server's found count is accepted, and the following empty page is EOF.
func TestReader_MultiPageFinalShortPage(t *testing.T) {
	var requests int
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		query := r.URL.Query()
		switch query.Get("offset") {
		case "4":
			if query.Get("limit") != "3" {
				t.Errorf("page 1 limit = %q, want 3", query.Get("limit"))
			}
			writeJSON(t, w, http.StatusOK, `{"found":6,"out_of":6,"page":1,"hits":[
				{"document":{"id":"a","title":"A"}},
				{"document":{"id":"b","title":"B"}}]}`)
		case "6":
			writeJSON(t, w, http.StatusOK, `{"found":6,"out_of":6,"page":1,"hits":[]}`)
		default:
			t.Errorf("unexpected offset %q", query.Get("offset"))
		}
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 3 })
	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(5).Offset(4).SelectIntoRecord(nil)
	reader, err := readerForQuery(t, db, query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var ids []string
	for {
		rec, nextErr := reader.Next()
		if errors.Is(nextErr, dal.ErrNoMoreRecords) {
			break
		}
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
		ids = append(ids, rec.Key().ID.(string))
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %v, want [a b]", ids)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func runNativeSearch(t *testing.T, body string, request SearchRequest) (SearchResult, error) {
	t.Helper()
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, body)
	})
	db := newTestDB(t, server, nil)
	searcher, ok := dal.As[NativeSearcher](db)
	if !ok {
		t.Fatal("NativeSearcher capability is missing")
	}
	return searcher.Search(context.Background(), request)
}

// TestSearch_SearchCutoffReporting decodes the Typesense search_cutoff response
// indicator strictly and never reports a cutoff as a complete result, even when
// the cutoff page returned no hits and found is zero.
func TestSearch_SearchCutoffReporting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantPartial  bool
		wantComplete bool
		wantReturned int
		wantFound    int
	}{
		{
			name:         "cutoff-with-zero-hits",
			body:         `{"found":0,"out_of":25,"page":1,"hits":[],"search_cutoff":true}`,
			wantPartial:  true,
			wantComplete: false,
			wantReturned: 0,
			wantFound:    0,
		},
		{
			name: "cutoff-with-hits",
			body: `{"found":2,"out_of":25,"page":1,"search_cutoff":true,"hits":[
				{"document":{"id":"a"}},{"document":{"id":"b"}}]}`,
			wantPartial:  true,
			wantComplete: false,
			wantReturned: 2,
			wantFound:    2,
		},
		{
			name: "cutoff-false-is-complete",
			body: `{"found":1,"out_of":25,"page":1,"search_cutoff":false,"hits":[
				{"document":{"id":"a"}}]}`,
			wantPartial:  false,
			wantComplete: true,
			wantReturned: 1,
			wantFound:    1,
		},
		{
			name: "cutoff-absent-is-complete",
			body: `{"found":1,"out_of":25,"page":1,"hits":[
				{"document":{"id":"a"}}]}`,
			wantPartial:  false,
			wantComplete: true,
			wantReturned: 1,
			wantFound:    1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runNativeSearch(t, tc.body, SearchRequest{Q: "*", Limit: 2})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if result.Partial != tc.wantPartial || result.Complete != tc.wantComplete {
				t.Fatalf("partial/complete = %v/%v, want %v/%v",
					result.Partial, result.Complete, tc.wantPartial, tc.wantComplete)
			}
			if result.Returned != tc.wantReturned || result.Found != tc.wantFound {
				t.Fatalf("returned/found = %d/%d, want %d/%d",
					result.Returned, result.Found, tc.wantReturned, tc.wantFound)
			}
		})
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"string", `{"found":0,"out_of":0,"page":1,"hits":[],"search_cutoff":"true"}`},
		{"null", `{"found":0,"out_of":0,"page":1,"hits":[],"search_cutoff":null}`},
		{"number", `{"found":0,"out_of":0,"page":1,"hits":[],"search_cutoff":1}`},
	} {
		t.Run("malformed-"+tc.name, func(t *testing.T) {
			if _, err := runNativeSearch(t, tc.body, SearchRequest{Q: "*", Limit: 1}); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("Search err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

// TestReadPaths_CutoffIsPartialRefusal proves every DALgo read path refuses a
// cutoff with a typed partial error rather than EOF, not-found or success.
func TestReadPaths_CutoffIsPartialRefusal(t *testing.T) {
	const body = `{"found":0,"out_of":10,"page":1,"hits":[],"search_cutoff":true}`
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, body)
	})
	db := newTestDB(t, server, nil)

	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(2).SelectIntoRecord(nil)
	reader, err := readerForQuery(t, db, query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if _, err = reader.Next(); !errors.Is(err, ErrPartialResult) {
		t.Fatalf("reader Next err = %v, want ErrPartialResult", err)
	}
	if errors.Is(err, dal.ErrNoMoreRecords) {
		t.Fatal("reader cutoff was mistaken for EOF")
	}
	_ = reader.Close()

	rec := record.NewRecordWithData(record.NewKeyWithID("books", "001"), &map[string]any{})
	if err = db.Get(context.Background(), rec); !errors.Is(err, ErrPartialResult) {
		t.Fatalf("Get err = %v, want ErrPartialResult", err)
	}
	if errors.Is(err, record.ErrRecordNotFound) {
		t.Fatal("Get cutoff was mistaken for not-found")
	}

	exists, err := db.Exists(context.Background(), record.NewKeyWithID("books", "001"))
	if !errors.Is(err, ErrPartialResult) || exists {
		t.Fatalf("Exists = %v, %v; want false, ErrPartialResult", exists, err)
	}

	batch := []record.Record{
		record.NewRecordWithData(record.NewKeyWithID("books", "001"), &map[string]any{}),
		record.NewRecordWithData(record.NewKeyWithID("books", "002"), &map[string]any{}),
	}
	if err = db.GetMulti(context.Background(), batch); !errors.Is(err, ErrPartialResult) {
		t.Fatalf("GetMulti err = %v, want ErrPartialResult", err)
	}
	if errors.Is(err, record.ErrRecordNotFound) {
		t.Fatal("GetMulti cutoff was mistaken for not-found")
	}
}

// TestReadBudgets_MaxRowBytesAllPaths proves MaxRowBytes is enforced by every
// read path before an over-budget row is exposed.
func TestReadBudgets_MaxRowBytesAllPaths(t *testing.T) {
	const documentJSON = `{"id":"a","title":"0123456789012345678901234567890123456789"}`
	const body = `{"found":1,"out_of":1,"page":1,"hits":[{"document":` + documentJSON + `}]}`
	size := int64(len(documentJSON))
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, body)
	})

	run := func(t *testing.T, db dal.DB) error {
		t.Helper()
		paths := []struct {
			name string
			run  func() error
		}{
			{"native-search", func() error {
				searcher, _ := dal.As[NativeSearcher](db)
				_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
				return err
			}},
			{"reader", func() error {
				query := dal.From(dal.NewRootCollectionRef("books", "")).
					NewQuery().Limit(1).SelectIntoRecord(nil)
				reader, err := readerForQuery(t, db, query)
				if err != nil {
					return err
				}
				defer func() { _ = reader.Close() }()
				_, err = reader.Next()
				return err
			}},
			{"recordset", func() error {
				query := dal.From(dal.NewRootCollectionRef("books", "")).
					NewQuery().Limit(1).SelectIntoRecordset()
				_, err := db.ExecuteQueryToRecordsetReader(context.Background(), query)
				return err
			}},
			{"get", func() error {
				rec := record.NewRecordWithData(record.NewKeyWithID("books", "a"), &map[string]any{})
				return db.Get(context.Background(), rec)
			}},
			{"exists", func() error {
				_, err := db.Exists(context.Background(), record.NewKeyWithID("books", "a"))
				return err
			}},
			{"get-multi", func() error {
				batch := []record.Record{
					record.NewRecordWithData(record.NewKeyWithID("books", "a"), &map[string]any{}),
				}
				return db.GetMulti(context.Background(), batch)
			}},
		}
		for _, path := range paths {
			t.Run(path.name, func(t *testing.T) {
				if err := path.run(); !errors.Is(err, ErrRowTooLarge) {
					t.Fatalf("%s err = %v, want ErrRowTooLarge", path.name, err)
				}
			})
		}
		return nil
	}

	t.Run("over-budget", func(t *testing.T) {
		db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowBytes = size - 1 })
		if err := run(t, db); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("at-boundary", func(t *testing.T) {
		db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowBytes = size })
		searcher, _ := dal.As[NativeSearcher](db)
		if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); err != nil {
			t.Fatalf("native search at row boundary: %v", err)
		}
		reader, err := readerForQuery(t, db, dal.From(dal.NewRootCollectionRef("books", "")).
			NewQuery().Limit(1).SelectIntoRecord(nil))
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		if _, err = reader.Next(); err != nil {
			t.Fatalf("reader at row boundary: %v", err)
		}
		_ = reader.Close()
	})
}

// TestReadBudgets_MaxRowsPerPageAllPaths proves the configured per-request row
// bound is honoured by native search and exact-id batches instead of silently
// ignored, while the structured reader keeps using it as its page size.
func TestReadBudgets_MaxRowsPerPageAllPaths(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "2" {
			t.Errorf("request limit = %q, want configured page size 2", r.URL.Query().Get("limit"))
		}
		writeJSON(t, w, http.StatusOK, `{"found":0,"out_of":0,"page":1,"hits":[]}`)
	})
	db := newTestDB(t, server, func(cfg *Config) {
		cfg.MaxRows = 10
		cfg.MaxRowsPerPage = 2
	})

	searcher, _ := dal.As[NativeSearcher](db)
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 3}); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("oversized native search err = %v, want ErrNotSupported", err)
	}
	batch := []record.Record{
		record.NewRecordWithData(record.NewKeyWithID("books", "a"), &map[string]any{}),
		record.NewRecordWithData(record.NewKeyWithID("books", "b"), &map[string]any{}),
		record.NewRecordWithData(record.NewKeyWithID("books", "c"), &map[string]any{}),
	}
	if err := db.GetMulti(context.Background(), batch); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("oversized batch err = %v, want ErrNotSupported", err)
	}
	if server.requests.Load() != 0 {
		t.Fatalf("refused requests performed %d I/O calls", server.requests.Load())
	}

	result, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 2})
	if err != nil {
		t.Fatalf("native search at page bound: %v", err)
	}
	if !result.Complete {
		t.Fatalf("native search at page bound = %+v", result)
	}
	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(5).SelectIntoRecord(nil)
	reader, err := readerForQuery(t, db, query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if _, err = reader.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
		t.Fatalf("reader Next err = %v, want ErrNoMoreRecords", err)
	}
	_ = reader.Close()
	if server.requests.Load() != 2 {
		t.Fatalf("allowed requests = %d, want 2", server.requests.Load())
	}
}

func TestNumeric_ExactDecimalInteger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		want    any
		wantErr bool
	}{
		{name: "plain-int", text: "9007199254740993", want: int64(9007199254740993)},
		{name: "exponent-int", text: "9007199254740993e0", want: int64(9007199254740993)},
		{name: "exponent-shifted", text: "9.007199254740993e15", want: int64(9007199254740993)},
		{name: "decimal-int", text: "9007199254740993.0", want: int64(9007199254740993)},
		{name: "integer-shifted-left", text: "90071992547409930e-1", want: int64(9007199254740993)},
		{name: "negative-exponent-int", text: "-9007199254740993E+0", want: int64(-9007199254740993)},
		{name: "uint64-max", text: "18446744073709551615e0", want: uint64(18446744073709551615)},
		{name: "uint64-overflow", text: "18446744073709551616e0", wantErr: true},
		{name: "int64-underflow", text: "-9223372036854775809", wantErr: true},
		{name: "float64-overflow", text: "1e400", wantErr: true},
		{name: "huge-positive-exponent", text: "1e999999", wantErr: true},
		{name: "zero-with-huge-exponent", text: "0.0e999999999", want: int64(0)},
		{name: "fraction", text: "1.5", want: 1.5},
		{name: "small-fraction", text: "0.1", want: 0.1},
		{name: "negative-zero", text: "-0.0", want: int64(0)},
		{name: "empty", text: "", wantErr: true},
		{name: "not-a-number", text: "abc", wantErr: true},
		{name: "double-dot", text: "1.2.3", wantErr: true},
		{name: "empty-exponent", text: "1e", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertJSONNumber(json.Number(tc.text))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("convertJSONNumber(%q) = %#v, want error", tc.text, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertJSONNumber(%q): %v", tc.text, err)
			}
			if got != tc.want {
				t.Fatalf("convertJSONNumber(%q) = %#v (%T), want %#v (%T)",
					tc.text, got, got, tc.want, tc.want)
			}
		})
	}
}

func TestNumeric_FormatJSONNumber(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		want    string
		wantErr bool
	}{
		{name: "exponent-int", text: "9007199254740993e0", want: "9007199254740993"},
		{name: "decimal-int", text: "9007199254740993.0", want: "9007199254740993"},
		{name: "shifted-int", text: "9.007199254740993e15", want: "9007199254740993"},
		{name: "exponent-short", text: "1e2", want: "100"},
		{name: "decimal-short", text: "1.0", want: "1"},
		{name: "fraction", text: "1.5", want: "1.5"},
		{name: "negative-zero", text: "-0.0", want: "0"},
		{name: "uint64-beyond-typesense-int64", text: "18446744073709551615e0", wantErr: true},
		{name: "uint64-overflow", text: "18446744073709551616e0", wantErr: true},
		{name: "invalid", text: "abc", wantErr: true},
		{name: "float-overflow", text: "1e400", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, class, err := formatJSONNumber(json.Number(tc.text))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("formatJSONNumber(%q) = %q, want error", tc.text, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("formatJSONNumber(%q): %v", tc.text, err)
			}
			if got != tc.want || class != classNumber {
				t.Fatalf("formatJSONNumber(%q) = %q, %v; want %q, number", tc.text, got, class, tc.want)
			}
		})
	}
}

// TestNumeric_RecordsAndRecordsetsPreserveIntegralExponent forms documents with
// exponent and decimal integral spellings and checks that every row surface
// keeps the exact integer instead of a float-rounded neighbour.
func TestNumeric_RecordsAndRecordsetsPreserveIntegralExponent(t *testing.T) {
	const body = `{"found":1,"out_of":1,"page":1,"hits":[{"document":{
		"id":"001",
		"big":9007199254740993e0,
		"decimal":9007199254740993.0,
		"negative":-9007199254740993E+0,
		"whole":1.0,
		"fraction":1.25,
		"u":18446744073709551615e0}}]}`
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, body)
	})
	db := newTestDB(t, server, nil)

	check := func(t *testing.T, document map[string]any) {
		t.Helper()
		if value, ok := document["big"].(int64); !ok || value != 9007199254740993 {
			t.Fatalf("big = %#v (%T), want exact int64", document["big"], document["big"])
		}
		if value, ok := document["decimal"].(int64); !ok || value != 9007199254740993 {
			t.Fatalf("decimal = %#v (%T), want exact int64", document["decimal"], document["decimal"])
		}
		if value, ok := document["negative"].(int64); !ok || value != -9007199254740993 {
			t.Fatalf("negative = %#v (%T), want exact int64", document["negative"], document["negative"])
		}
		if value, ok := document["whole"].(int64); !ok || value != 1 {
			t.Fatalf("whole = %#v (%T), want int64 1", document["whole"], document["whole"])
		}
		if value, ok := document["fraction"].(float64); !ok || value != 1.25 {
			t.Fatalf("fraction = %#v (%T), want float64 1.25", document["fraction"], document["fraction"])
		}
		if value, ok := document["u"].(uint64); !ok || value != 18446744073709551615 {
			t.Fatalf("u = %#v (%T), want uint64 max", document["u"], document["u"])
		}
	}

	searcher, _ := dal.As[NativeSearcher](db)
	result, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	check(t, result.Hits[0].Document)

	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := readerForQuery(t, db, query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	rec, err := reader.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	check(t, rec.Data().(map[string]any))
	_ = reader.Close()

	recordsetQuery := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(1).SelectIntoRecordset()
	recordsetReader, err := db.ExecuteQueryToRecordsetReader(context.Background(), recordsetQuery)
	if err != nil {
		t.Fatalf("recordset reader: %v", err)
	}
	rs := recordsetReader.Recordset()
	row := rs.GetRow(0)
	recordsetDocument := make(map[string]any, len(rs.Columns()))
	for _, column := range rs.Columns() {
		value, getErr := row.GetValueByName(column.Name(), rs)
		if getErr != nil {
			t.Fatalf("GetValueByName(%q): %v", column.Name(), getErr)
		}
		recordsetDocument[column.Name()] = value
	}
	check(t, recordsetDocument)
}

func TestNumeric_OutOfRangeDocumentIsRefused(t *testing.T) {
	const body = `{"found":1,"out_of":1,"page":1,"hits":[
		{"document":{"id":"a","big":18446744073709551616e0}}]}`
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, body)
	})
	db := newTestDB(t, server, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("native Search err = %v, want ErrMalformedResponse", err)
	}
	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := readerForQuery(t, db, query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if _, err = reader.Next(); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("reader Next err = %v, want ErrMalformedResponse", err)
	}
	_ = reader.Close()
}

func TestNumeric_FilterKeepsIntegralExponent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator dal.Operator
		number   string
		want     string
		wantErr  bool
	}{
		{name: "exponent", operator: dal.Equal, number: "9007199254740993e0", want: "big:=9007199254740993"},
		{name: "shifted", operator: dal.GreaterThen, number: "9.007199254740993e15", want: "big:>9007199254740993"},
		{name: "decimal", operator: dal.Equal, number: "9007199254740993.0", want: "big:=9007199254740993"},
		{name: "short-exponent", operator: dal.Equal, number: "1e2", want: "big:=100"},
		{name: "fraction", operator: dal.GreaterThen, number: "1.5", want: "big:>1.5"},
		{name: "uint64-overflow", operator: dal.Equal, number: "18446744073709551616e0", wantErr: true},
		{name: "beyond-typesense-int64", operator: dal.Equal, number: "18446744073709551615e0", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := compileCondition(dal.Comparison{
				Left:     dal.Field("big"),
				Operator: tc.operator,
				Right:    dal.Constant{Value: json.Number(tc.number)},
			}, 0, new(int), 1024)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("compile = %q, want error", text)
				}
				return
			}
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if text != tc.want {
				t.Fatalf("compile = %q, want %q", text, tc.want)
			}
		})
	}
}

func TestNumeric_NativeFacetStatsKeepExactClasses(t *testing.T) {
	const body = `{"found":1,"out_of":1,"page":1,"hits":[
		{"document":{"id":"a","price":9007199254740993}}],
		"facet_counts":[{"field_name":"price","counts":[{"value":"x","count":1}],
		"stats":{"min":9007199254740993,"max":18446744073709551615,
		"sum":9007199254740993e0,"avg":1.5,"total_values":2}}]}`
	result, err := runNativeSearch(t, body, SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"price"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Facets) != 1 || result.Facets[0].Stats == nil {
		t.Fatalf("facets = %+v", result.Facets)
	}
	stats := result.Facets[0].Stats
	if value, ok := stats.Min.(int64); !ok || value != 9007199254740993 {
		t.Fatalf("stats.Min = %#v (%T), want exact int64", stats.Min, stats.Min)
	}
	if value, ok := stats.Max.(uint64); !ok || value != 18446744073709551615 {
		t.Fatalf("stats.Max = %#v (%T), want exact uint64", stats.Max, stats.Max)
	}
	if value, ok := stats.Sum.(int64); !ok || value != 9007199254740993 {
		t.Fatalf("stats.Sum = %#v (%T), want exact int64", stats.Sum, stats.Sum)
	}
	if value, ok := stats.Avg.(float64); !ok || value != 1.5 {
		t.Fatalf("stats.Avg = %#v (%T), want float64 1.5", stats.Avg, stats.Avg)
	}
	if stats.TotalValues != 2 {
		t.Fatalf("stats.TotalValues = %d, want 2", stats.TotalValues)
	}

	const malformedStats = `{"found":1,"out_of":1,"page":1,"hits":[
		{"document":{"id":"a","price":1}}],
		"facet_counts":[{"field_name":"price","counts":[{"value":"x","count":1}],
		"stats":{"min":"x"}}]}`
	if _, err = runNativeSearch(t, malformedStats, SearchRequest{Q: "*", Limit: 1, FacetBy: []string{"price"}}); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("malformed stats err = %v, want ErrMalformedResponse", err)
	}
}

// TestSearch_MaxRowsPerPageIsHonest keeps the request bound visible in the
// encoded query, including a limit equal to the configured page size.
func TestSearch_MaxRowsPerPageIsHonest(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit = %q, want 2", got)
		}
		writeJSON(t, w, http.StatusOK, `{"found":0,"out_of":0,"page":1,"hits":[]}`)
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 2 })
	searcher, _ := dal.As[NativeSearcher](db)
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 2}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 3}); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("oversized Search err = %v, want ErrNotSupported", err)
	}
	if server.requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", server.requests.Load())
	}
}

func TestReader_OffsetParsingIsBounded(t *testing.T) {
	// Keep the offset bound visible: offset beyond MaxOffset is refused before
	// any page request, so an out-of-range empty page cannot mask a budget bug.
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be issued")
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxOffset = 10 })
	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(1).Offset(11).SelectIntoRecord(nil)
	if _, err := readerForQuery(t, db, query); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
	if server.requests.Load() != 0 {
		t.Fatalf("refused query performed %d I/O calls", server.requests.Load())
	}
}
