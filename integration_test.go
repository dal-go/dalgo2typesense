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
		if def.Name != "books" || len(def.Fields) < 10 {
			t.Fatalf("schema = %+v", def)
		}
		if def.PrimaryKey[0] != "id" || def.SourceDefinition.Dialect != "typesense" {
			t.Fatalf("identity/provenance = %+v", def)
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
		for _, id := range ids {
			if id != "004" && id != "006" {
				t.Fatalf("unexpected cyberpunk id %q", id)
			}
		}

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
		for _, rec := range records {
			if rec.Key().ID == "001" {
				t.Fatal("structured query let curated 001 bypass filter_by")
			}
		}
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
		reader, err := pureDB.ExecuteQueryToRecordsReader(context.Background(), q)
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
	db, err := NewDB(context.Background(), Config{
		Endpoint:          f.endpoint,
		Collection:        collection,
		AllowLoopbackHTTP: true,
		Headers: func(context.Context) (map[string]string, error) {
			return map[string]string{"X-TYPESENSE-API-KEY": key}, nil
		},
		MaxRowsPerPage: 3,
	})
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

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
