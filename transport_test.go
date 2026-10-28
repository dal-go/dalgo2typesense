package dalgo2typesense

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
)

func TestMetadata_PureSearchCredentialRefusesInsteadOfEmpty(t *testing.T) {
	var searchRequests atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/collections/books/documents/search" {
			searchRequests.Add(1)
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"001","title":"Dune"}}]}`)
			return
		}
		writeJSON(t, w, http.StatusForbidden, `{"message":"forbidden"}`)
	})
	db := newTestDB(t, server, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); err != nil {
		t.Fatalf("pure search key must run a native search: %v", err)
	}
	reader, ok := dal.As[dbschema.SchemaReader](db)
	if !ok {
		t.Fatal("SchemaReader capability is missing")
	}
	collections, err := reader.ListCollections(context.Background(), nil)
	if !errors.Is(err, ErrMetadataForbidden) {
		t.Fatalf("ListCollections err = %v, want ErrMetadataForbidden", err)
	}
	if collections != nil {
		t.Fatalf("forbidden metadata returned %d collections", len(collections))
	}
	if _, err = reader.DescribeCollection(context.Background(), ptrRef("books")); !errors.Is(err, ErrMetadataForbidden) {
		t.Fatalf("DescribeCollection err = %v, want ErrMetadataForbidden", err)
	}
	if searchRequests.Load() != 1 {
		t.Fatalf("search requests = %d", searchRequests.Load())
	}
}

func ptrRef(name string) *dal.CollectionRef {
	ref := dal.NewRootCollectionRef(name, "")
	return &ref
}

func TestMetadata_AuthorizedCredentialDiscoversSchema(t *testing.T) {
	const collectionJSON = `{
		"name":"books","num_documents":3,"default_sorting_field":"rank",
		"fields":[
			{"name":"title","type":"string","facet":false},
			{"name":"genre","type":"string","facet":true},
			{"name":"rank","type":"int64","sort":true},
			{"name":"big","type":"int64"},
			{"name":"payload","type":"object","optional":true}
		]}`
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/collections":
			writeJSON(t, w, http.StatusOK, `[`+collectionJSON+`]`)
		case "/collections/books":
			writeJSON(t, w, http.StatusOK, collectionJSON)
		default:
			http.NotFound(w, r)
		}
	})
	db := newTestDB(t, server, nil)
	reader, _ := dal.As[dbschema.SchemaReader](db)
	collections, err := reader.ListCollections(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(collections) != 1 || collections[0].Name() != "books" {
		t.Fatalf("collections = %+v", collections)
	}
	def, err := reader.DescribeCollection(context.Background(), ptrRef("books"))
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if def.Name != "books" || len(def.Fields) != 5 {
		t.Fatalf("def = %+v", def)
	}
	if def.PrimaryKey[0] != "id" {
		t.Fatalf("primary key = %v", def.PrimaryKey)
	}
	if def.Fields[0].Type != dbschema.String || def.Fields[2].Type != dbschema.Int {
		t.Fatalf("portable types = %v, %v", def.Fields[0].Type, def.Fields[2].Type)
	}
	if def.Fields[4].Type != dbschema.Null || def.SourceDefinition.Columns[4].DeclaredType != "object" {
		t.Fatalf("object metadata lost: %+v", def.Fields[4])
	}
	if !def.Fields[4].Nullable {
		t.Fatal("optional field is not nullable")
	}
	if def.SourceDefinition == nil || def.SourceDefinition.Dialect != "typesense" {
		t.Fatalf("source provenance missing: %+v", def.SourceDefinition)
	}
	indexes, err := reader.ListIndexes(context.Background(), ptrRef("books"))
	if err != nil || len(indexes) != 0 {
		t.Fatalf("ListIndexes = %v, %v", indexes, err)
	}
}

func TestRedirectRefusedAndCredentialNotForwarded(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		writeJSON(t, w, http.StatusOK, `{"found":0,"out_of":0,"page":1,"hits":[]}`)
	}))
	defer other.Close()
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	})
	db := newTestDB(t, server, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("err = %v, want ErrRedirectRefused", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times", redirected.Load())
	}
}

func TestHeadersAreRefreshedPerRequestAndUnsafeHeadersRefused(t *testing.T) {
	var keyIndex atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-TYPESENSE-API-KEY"); got != "key-1" && got != "key-2" {
			t.Errorf("unexpected key header")
		}
		writeJSON(t, w, http.StatusOK, `{"found":0,"out_of":0,"page":1,"hits":[]}`)
	})
	db := newTestDB(t, server, func(cfg *Config) {
		cfg.Headers = func(context.Context) (map[string]string, error) {
			n := keyIndex.Add(1)
			return map[string]string{"X-TYPESENSE-API-KEY": "key-" + string(rune('0'+n))}, nil
		}
	})
	searcher, _ := dal.As[NativeSearcher](db)
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if keyIndex.Load() != 2 {
		t.Fatalf("header provider calls = %d, want 2", keyIndex.Load())
	}

	unsafe := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsafe header request reached the server")
	})
	unsafeDB := newTestDB(t, unsafe, func(cfg *Config) {
		cfg.Headers = func(context.Context) (map[string]string, error) {
			return map[string]string{"X-TYPESENSE-API-KEY": "k\r\nInjected: x"}, nil
		}
	})
	unsafeSearcher, _ := dal.As[NativeSearcher](unsafeDB)
	if _, err := unsafeSearcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); !errors.Is(err, ErrUnsafeValue) {
		t.Fatalf("err = %v, want ErrUnsafeValue", err)
	}
}

func TestCancellationAndClose(t *testing.T) {
	started := make(chan struct{})
	disconnected := make(chan struct{})
	release := make(chan struct{})
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-release:
		}
	})
	db := newTestDB(t, server, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := searcher.Search(ctx, SearchRequest{Q: "*", Limit: 1})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("err = %v, want ErrCanceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled search did not return")
	}
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not observe the client disconnect")
	}
	close(release)

	closer, ok := dal.As[io.Closer](db)
	if !ok {
		t.Fatal("Close capability is missing")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("search after close err = %v, want ErrClosed", err)
	}
}

func TestGetExistsGetMulti_ExactIDSearch(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		filter := query.Get("filter_by")
		if query.Get("q") != "*" || query.Get("enable_curations") != "false" || query.Get("filter_curated_hits") != "true" {
			t.Errorf("unexpected lookup query: %v", query)
		}
		switch filter {
		case "id:=[`001`]":
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":4,"page":1,"hits":[
				{"document":{"id":"001","title":"Dune","n":9007199254740993}}]}`)
		case "id:=[`001`,`missing`]":
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":4,"page":1,"hits":[
				{"document":{"id":"001","title":"Dune","n":9007199254740993}}]}`)
		default:
			t.Errorf("unexpected filter_by: %q", filter)
			writeJSON(t, w, http.StatusOK, `{"found":0,"out_of":0,"page":1,"hits":[]}`)
		}
	})
	db := newTestDB(t, server, nil)
	rec := record.NewRecordWithData(record.NewKeyWithID("books", "001"), &map[string]any{})
	if err := db.Get(context.Background(), rec); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Data().(*map[string]any) == nil || (*rec.Data().(*map[string]any))["title"] != "Dune" {
		t.Fatalf("Get data = %#v", rec.Data())
	}
	exists, err := db.Exists(context.Background(), record.NewKeyWithID("books", "001"))
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	missing := record.NewRecordWithData(record.NewKeyWithID("books", "missing"), &map[string]any{})
	err = db.GetMulti(context.Background(), []record.Record{rec, missing})
	var multi *GetMultiError
	if !errors.As(err, &multi) || multi.Index != 1 {
		t.Fatalf("GetMulti err = %v", err)
	}
	if !errors.Is(err, record.ErrRecordNotFound) {
		t.Fatalf("GetMulti err = %v, want ErrRecordNotFound", err)
	}
	if _, err = db.Exists(context.Background(), record.NewKeyWithID("books", 42)); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("non-string key err = %v", err)
	}
}

func TestBadKeyAndForbiddenCollectionAreClassified(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, `{"message":"bad key"}`)
	})
	db := newTestDB(t, server, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if strings.Contains(err.Error(), "bad key") {
		t.Fatal("provider error text leaked")
	}
}

func TestReadonlyTransactionRefusedBeforeIO(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("refused transaction performed I/O")
	})
	db := newTestDB(t, server, nil)
	called := false
	err := db.RunReadonlyTransaction(context.Background(), func(context.Context, dal.ReadTransaction) error {
		called = true
		return nil
	})
	if !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
	if called {
		t.Fatal("readonly transaction callback ran")
	}
	if server.requests.Load() != 0 {
		t.Fatalf("refused transaction performed %d I/O calls", server.requests.Load())
	}
}
