package dalgo2typesense

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
)

type testServer struct {
	*httptest.Server
	requests atomic.Int32
}

func newTestServer(t *testing.T, handler http.HandlerFunc) *testServer {
	t.Helper()
	s := &testServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func newTestDB(t *testing.T, server *testServer, mutate func(*Config)) dal.DB {
	t.Helper()
	cfg := Config{
		Endpoint:          server.URL,
		Collection:        "books",
		AllowLoopbackHTTP: true,
		Headers: func(context.Context) (map[string]string, error) {
			return map[string]string{"X-TYPESENSE-API-KEY": "test-key"}, nil
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

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write body: %v", err)
	}
}

func TestNewDB_Validation(t *testing.T) {
	headers := func(context.Context) (map[string]string, error) {
		return map[string]string{"X-TYPESENSE-API-KEY": "k"}, nil
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"missing-endpoint", Config{Headers: headers}},
		{"endpoint-path", Config{Endpoint: "https://typesense.example.com/api", Headers: headers}},
		{"endpoint-credentials", Config{Endpoint: "https://user:pass@typesense.example.com", Headers: headers}},
		{"plain-http-public", Config{Endpoint: "http://typesense.example.com", Headers: headers}},
		{"plain-http-loopback-not-allowed", Config{Endpoint: "http://127.0.0.1:8108", Headers: headers}},
		{"missing-headers", Config{Endpoint: "https://typesense.example.com"}},
		{"bad-collection", Config{Endpoint: "https://typesense.example.com", Collection: "bad/collection", Headers: headers}},
		{"negative-bound", Config{Endpoint: "https://typesense.example.com", Headers: headers, MaxRows: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDB(context.Background(), tc.cfg); err == nil {
				t.Fatal("expected NewDB to reject config")
			}
		})
	}
}

func TestSearch_OutboundPolicyAndLosslessDocument(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collections/books/documents/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		query := r.URL.Query()
		if got := query.Get("enable_curations"); got != "false" {
			t.Errorf("enable_curations = %q, want false", got)
		}
		if got := query.Get("filter_curated_hits"); got != "true" {
			t.Errorf("filter_curated_hits = %q, want true", got)
		}
		if got := query.Get("q"); got != "dune" {
			t.Errorf("q = %q", got)
		}
		if got := query.Get("query_by"); got != "title" {
			t.Errorf("query_by = %q", got)
		}
		writeJSON(t, w, http.StatusOK, `{
			"found": 1, "out_of": 5, "page": 1, "search_time_ms": 3,
			"hits": [{
				"document": {"id": "001", "title": "Dune", "n": 9007199254740993,
					"u": 18446744073709551615, "f": 1.5, "nil": null,
					"arr": [1, "x", true], "obj": {"k": true}},
				"text_match": 42
			}],
			"facet_counts": []
		}`)
	})
	db := newTestDB(t, server, nil)
	searcher, ok := dal.As[NativeSearcher](db)
	if !ok {
		t.Fatal("NativeSearcher capability is missing")
	}
	result, err := searcher.Search(context.Background(), SearchRequest{
		Collection: "books", Q: "dune", QueryBy: []string{"title"}, Limit: 1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.Returned != 1 || result.Found != 1 || result.OutOf != 5 {
		t.Fatalf("unexpected counts: %+v", result)
	}
	if result.Provenance != ProvenanceSearch || result.Partial || !result.Complete {
		t.Fatalf("unexpected provenance/state: %+v", result)
	}
	document := result.Hits[0].Document
	if document["id"] != "001" {
		t.Fatalf("native string id was not preserved: %#v", document["id"])
	}
	if _, ok := document["n"].(int64); !ok {
		t.Fatalf("int64 class not preserved: %T", document["n"])
	}
	if _, ok := document["u"].(uint64); !ok {
		t.Fatalf("uint64 class not preserved: %T", document["u"])
	}
	if _, ok := document["f"].(float64); !ok {
		t.Fatalf("float class not preserved: %T", document["f"])
	}
	if document["nil"] != nil {
		t.Fatal("null was not preserved")
	}
	if _, ok := document["arr"].([]any); !ok {
		t.Fatalf("array class not preserved: %T", document["arr"])
	}
	if _, ok := document["obj"].(map[string]any); !ok {
		t.Fatalf("object class not preserved: %T", document["obj"])
	}
}

func TestSearch_RejectsUnsafeAndUnboundedRequests(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be issued")
	})
	db := newTestDB(t, server, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	for _, tc := range []struct {
		name string
		req  SearchRequest
	}{
		{"zero-limit", SearchRequest{Q: "*", Limit: 0}},
		{"text-without-query-by", SearchRequest{Q: "dune", Limit: 1}},
		{"collection-mismatch", SearchRequest{Collection: "other", Q: "*", Limit: 1}},
		{"unsafe-filter", SearchRequest{Q: "*", Limit: 1, FilterBy: "title:$(x)"}},
		{"unbalanced-filter", SearchRequest{Q: "*", Limit: 1, FilterBy: "title:=[`unclosed"}},
		{"nested-filter", SearchRequest{Q: "*", Limit: 1, FilterBy: "author.{name:=x}"}},
		{"too-many-sorts", SearchRequest{Q: "*", Limit: 1, SortBy: []SortField{
			{Field: "a"}, {Field: "b"}, {Field: "c"}, {Field: "d"},
		}}},
		{"id-sort", SearchRequest{Q: "*", Limit: 1, SortBy: []SortField{{Field: "id"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := searcher.Search(context.Background(), tc.req); err == nil {
				t.Fatal("expected request rejection")
			}
		})
	}
	if server.requests.Load() != 0 {
		t.Fatalf("rejected requests performed %d I/O calls", server.requests.Load())
	}
}

func TestStructuredQuery_FlagsProjectionAndPaging(t *testing.T) {
	var requests atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		query := r.URL.Query()
		if query.Get("q") != "*" {
			t.Errorf("q = %q, want *", query.Get("q"))
		}
		if query.Get("enable_curations") != "false" || query.Get("filter_curated_hits") != "true" {
			t.Errorf("curation policy flags missing: %v", query)
		}
		if query.Get("num_typos") != "0" || query.Get("prefix") != "false" ||
			query.Get("drop_tokens_threshold") != "0" || query.Get("typo_tokens_threshold") != "0" {
			t.Errorf("structured text fallbacks not disabled: %v", query)
		}
		if query.Get("filter_by") != "(author:=`Frank Herbert` && year:>=1965)" {
			t.Errorf("filter_by = %q", query.Get("filter_by"))
		}
		if query.Get("include_fields") != "id,title,rank,author,year" {
			t.Errorf("include_fields = %q", query.Get("include_fields"))
		}
		switch n {
		case 1:
			if query.Get("offset") != "" || query.Get("limit") != "2" {
				t.Errorf("page 1 paging: offset=%q limit=%q", query.Get("offset"), query.Get("limit"))
			}
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":10,"page":1,"hits":[
				{"document":{"id":"001","title":"Dune","rank":1,"author":"Frank Herbert","year":1965}},
				{"document":{"id":"002","title":"Dune Messiah","rank":2,"author":"Frank Herbert","year":1969}}]}`)
		case 2:
			if query.Get("offset") != "2" || query.Get("limit") != "1" {
				t.Errorf("page 2 paging: offset=%q limit=%q", query.Get("offset"), query.Get("limit"))
			}
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":10,"page":1,"hits":[
				{"document":{"id":"003","title":"Children of Dune","rank":3,"author":"Frank Herbert","year":1976}}]}`)
		default:
			t.Errorf("unexpected request %d", n)
		}
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 2 })
	q := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().
		Where(dal.NewGroupCondition(dal.And,
			dal.Field("author").EqualTo("Frank Herbert"),
			dal.WhereField("year", dal.GreaterOrEqual, 1965))).
		OrderBy(dal.AscendingField("rank")).
		Limit(3).
		SelectColumns(
			dal.Column{Expression: dal.Field("title")},
			dal.Column{Expression: dal.Field("rank")},
		)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	if _, err := reader.Cursor(); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("Cursor err = %v, want ErrNotSupported", err)
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
		data := rec.Data().(map[string]any)
		if len(data) != 2 {
			t.Fatalf("projected width = %d, want 2: %#v", len(data), data)
		}
		if _, leaked := data["id"]; leaked {
			t.Fatal("undeclared helper id leaked into projected output")
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Join(ids, ",") != "001,002,003" {
		t.Fatalf("ids = %v", ids)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestStructuredQuery_RefusesUnsupportedBeforeIO(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("refused query performed I/O")
	})
	db := newTestDB(t, server, nil)
	base := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().Limit(1)
	for _, tc := range []struct {
		name  string
		query dal.Query
	}{
		{"no-limit", dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().SelectIntoRecord(nil)},
		{"other-collection", dal.From(dal.NewRootCollectionRef("other", "")).NewQuery().Limit(1).SelectIntoRecord(nil)},
		{"is-null", base.Where(dal.Field("deletedAt").IsNull()).SelectIntoRecord(nil)},
		{"is-not-null", base.Where(dal.Field("deletedAt").IsNotNull()).SelectIntoRecord(nil)},
		{"nested-field", base.Where(dal.Field("author.name").EqualTo("x")).SelectIntoRecord(nil)},
		{"unsupported-value", base.Where(dal.Comparison{
			Left:     dal.Field("tags"),
			Operator: dal.Equal,
			Right:    dal.Constant{Value: map[string]any{"a": 1}},
		}).SelectIntoRecord(nil)},
		{"unsupported-operator", base.Where(dal.WhereField("rank", dal.Operator("LIKE"), 1)).SelectIntoRecord(nil)},
		{"wildcard", base.SelectColumns(dal.AllColumnsExcept())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecuteQueryToRecordsReader(context.Background(), tc.query); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
	if server.requests.Load() != 0 {
		t.Fatalf("refusals performed %d I/O calls", server.requests.Load())
	}
}

func TestStructuredFilter_Escaping(t *testing.T) {
	if _, err := compileCondition(dal.Field("title").EqualTo("a`b"), 0, new(int), 1024); err == nil {
		t.Fatal("backtick constant must be refused")
	}
	inArray := func(values any) dal.Condition {
		return dal.Comparison{Left: dal.Field("title"), Operator: dal.In, Right: dal.Array{Value: values}}
	}
	if _, err := compileCondition(inArray("../../etc"), 0, new(int), 1024); err == nil {
		t.Fatal("In needs an array value")
	}
	text, err := compileCondition(inArray([]string{"a b", "c(d)"}), 0, new(int), 1024)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if text != "title:=[`a b`,`c(d)`]" {
		t.Fatalf("escaped filter = %q", text)
	}
}

func TestRecordset_HeterogeneousNativeClasses(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":2,"out_of":2,"page":1,"hits":[
			{"document":{"id":"a","flag":true,"n":9007199254740993,"nil":null}},
			{"document":{"id":"b","flag":false,"n":-2,"nil":null}}]}`)
	})
	db := newTestDB(t, server, nil)
	q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().Limit(2).
		SelectIntoRecordset(recordsetName("rs"))
	reader, err := db.ExecuteQueryToRecordsetReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
	}
	rs := reader.Recordset()
	if rs.RowsCount() != 2 {
		t.Fatalf("rows = %d", rs.RowsCount())
	}
	value, err := rs.GetRow(0).GetValueByName("n", rs)
	if err != nil {
		t.Fatalf("GetValueByName: %v", err)
	}
	if _, ok := value.(int64); !ok {
		t.Fatalf("recordset int class = %T", value)
	}
	flag, err := rs.GetRow(1).GetValueByName("flag", rs)
	if err != nil {
		t.Fatalf("GetValueByName flag: %v", err)
	}
	if flag != false {
		t.Fatalf("bool value = %#v", flag)
	}
	column := rs.Columns()[0]
	if column.ValueType().Kind() != reflect.Interface {
		t.Fatalf("column value type = %v, want interface", column.ValueType())
	}
}

func recordsetName(name string) recordset.Option {
	return recordset.WithName(name)
}

func TestRecordset_Malformed200IsErrorNotEOF(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":0,"out_of":0,"page":1,"hits":null}`)
	})
	db := newTestDB(t, server, nil)
	q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	_, err = reader.Next()
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
}

func TestPartialResultIsNotCleanEOF(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":5,"out_of":5,"page":1,"hits":[
			{"document":{"id":"a","title":"A"}}]}`)
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 2 })
	q := dal.From(dal.NewRootCollectionRef("books", "")).NewQuery().Limit(2).SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	_, err = reader.Next()
	if !errors.Is(err, ErrPartialResult) {
		t.Fatalf("err = %v, want ErrPartialResult", err)
	}
}
