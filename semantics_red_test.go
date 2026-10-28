package dalgo2typesense

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
)

func structuredBooksQuery(where dal.Condition, limit, offset int, columns ...string) dal.Query {
	var q dal.IQueryBuilder = dal.From(dal.NewRootCollectionRef("books", "")).NewQuery()
	if where != nil {
		q = q.Where(where)
	}
	q = q.Limit(limit).Offset(offset)
	if len(columns) > 0 {
		selected := make([]dal.Column, 0, len(columns))
		for _, name := range columns {
			selected = append(selected, dal.Column{Expression: dal.Field(name)})
		}
		return q.SelectColumns(selected...)
	}
	return q.SelectIntoRecord(nil)
}

// nextRow returns the first row or the terminal error. A successful row is a
// failure for every false-positive case in this file.
func nextRow(t *testing.T, reader dal.RecordsReader) {
	t.Helper()
	rec, err := reader.Next()
	if err == nil {
		t.Fatalf("mismatched candidate was exposed as row %v: %#v", rec.Key().ID, rec.Data())
	}
	if !errors.Is(err, ErrSemanticMismatch) || !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("Next err = %v, want ErrSemanticMismatch and dal.ErrNotSupported", err)
	}
}

func TestStructuredSemantics_StringEqualFalsePositiveIsRefused(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":2,"out_of":2,"page":1,"hits":[
			{"document":{"id":"a","label":"classic"}},
			{"document":{"id":"b","label":"Classic"}}]}`)
	})
	db := newTestDB(t, server, nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(dal.Field("label").EqualTo("classic"), 2, 0))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextRow(t, reader)
}

func TestStructuredSemantics_ArrayEqualAndNumericRangeFalsePositiveIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where dal.Condition
		body  string
	}{
		{
			name:  "array-equal",
			where: dal.Field("tags").EqualTo("classic"),
			body: `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"a","tags":["classic","desert"]}}]}`,
		},
		{
			name:  "numeric-range-array",
			where: dal.WhereField("nums", dal.GreaterThen, 50),
			body: `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"a","nums":[5,100]}}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, tc.body)
			})
			db := newTestDB(t, server, nil)
			reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
				structuredBooksQuery(tc.where, 1, 0))
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			defer func() { _ = reader.Close() }()
			nextRow(t, reader)
		})
	}
}

func TestStructuredSemantics_StringInFalsePositiveIsRefused(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
			{"document":{"id":"b","label":"CLASSIC"}}]}`)
	})
	db := newTestDB(t, server, nil)
	condition := dal.Comparison{
		Left:     dal.Field("label"),
		Operator: dal.In,
		Right:    dal.Array{Value: []string{"classic"}},
	}
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(condition, 1, 0))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextRow(t, reader)
}

// TestStructuredSemantics_LaterPageMismatchHasNoPrefix proves the whole
// filtered candidate window is buffered and validated before the first row.
func TestStructuredSemantics_LaterPageMismatchHasNoPrefix(t *testing.T) {
	var requests int
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Query().Get("offset") {
		case "":
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":3,"page":1,"hits":[
				{"document":{"id":"a","label":"classic"}},
				{"document":{"id":"b","label":"classic"}}]}`)
		case "2":
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":3,"page":1,"hits":[
				{"document":{"id":"c","label":"Classic"}}]}`)
		default:
			t.Errorf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 2 })
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(dal.Field("label").EqualTo("classic"), 3, 0))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextRow(t, reader)
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

// TestStructuredSemantics_FilteredOffsetValidatesSkippedPrefix proves a
// mismatching candidate before the requested offset cannot be skipped.
func TestStructuredSemantics_FilteredOffsetValidatesSkippedPrefix(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":2,"out_of":2,"page":1,"hits":[
			{"document":{"id":"a","label":"Classic"}},
			{"document":{"id":"b","label":"classic"}}]}`)
	})
	db := newTestDB(t, server, nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(dal.Field("label").EqualTo("classic"), 1, 1))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextRow(t, reader)
}

// TestStructuredSemantics_FilteredOffsetStartsAtZero pins the approved
// filtered-prefix contract: a filtered request never jumps over candidates.
func TestStructuredSemantics_FilteredOffsetStartsAtZero(t *testing.T) {
	var offsets []string
	var limits []string
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		offsets = append(offsets, query.Get("offset"))
		limits = append(limits, query.Get("limit"))
		switch len(offsets) {
		case 1:
			writeJSON(t, w, http.StatusOK, `{"found":4,"out_of":4,"page":1,"hits":[
				{"document":{"id":"a","label":"classic"}},
				{"document":{"id":"b","label":"classic"}}]}`)
		case 2:
			writeJSON(t, w, http.StatusOK, `{"found":4,"out_of":4,"page":1,"hits":[
				{"document":{"id":"c","label":"classic"}}]}`)
		default:
			t.Errorf("unexpected request %d", len(offsets))
		}
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 2 })
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(dal.Field("label").EqualTo("classic"), 2, 1))
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
	if len(ids) != 2 || ids[0] != "b" || ids[1] != "c" {
		t.Fatalf("ids = %v, want [b c]", ids)
	}
	if len(offsets) != 2 || offsets[0] != "" || offsets[1] != "2" {
		t.Fatalf("offsets = %v, want first offset empty (zero) and second 2", offsets)
	}
	if len(limits) != 2 || limits[0] != "2" || limits[1] != "1" {
		t.Fatalf("limits = %v, want [2 1]", limits)
	}
}

func TestStructuredSemantics_StructuredNotInRefusedBeforeIO(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("structured NOT IN performed I/O")
	})
	db := newTestDB(t, server, nil)
	notIn := dal.Comparison{
		Left:     dal.Field("tags"),
		Operator: dal.NotIn,
		Right:    dal.Array{Value: []string{"classic"}},
	}
	where := dal.NewGroupCondition(dal.Or, notIn, dal.Field("year").EqualTo(1969))
	if _, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(where, 1, 0)); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("err = %v, want dal.ErrNotSupported", err)
	}
	if requests := server.requests.Load(); requests != 0 {
		t.Fatalf("refused NOT IN performed %d I/O calls", requests)
	}
}

func TestStructuredSemantics_ImpossibleFilteredWindowRefusedBeforeIO(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("impossible filtered window performed I/O")
	})
	db := newTestDB(t, server, func(cfg *Config) {
		cfg.MaxRowsPerPage = 2
		cfg.MaxPages = 2
	})
	if _, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(dal.Field("label").EqualTo("classic"), 3, 3)); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("err = %v, want dal.ErrNotSupported", err)
	}
	if requests := server.requests.Load(); requests != 0 {
		t.Fatalf("refused window performed %d I/O calls", requests)
	}
}

func TestStructuredSemantics_HelperFieldsAreBoundedAndHidden(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("helper-bound refusal performed I/O")
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxColumns = 2 })
	condition := dal.NewGroupCondition(dal.And,
		dal.Field("author").EqualTo("Frank Herbert"),
		dal.WhereField("year", dal.GreaterOrEqual, 1965))
	if _, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(condition, 1, 0, "title")); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("err = %v, want dal.ErrNotSupported", err)
	}
	if requests := server.requests.Load(); requests != 0 {
		t.Fatalf("refused helper bound performed %d I/O calls", requests)
	}
}

func TestStructuredSemantics_SecureReadSessionResidualCannotLeak(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"found":2,"out_of":2,"page":1,"hits":[
			{"document":{"id":"a","label":"classic"}},
			{"document":{"id":"b","label":"Classic"}}]}`)
	})
	db := newTestDB(t, server, nil)
	policy := access.MustPolicy("labels",
		access.Collection("books", access.Allow(access.Query, "rows").Where(
			dal.Field("label").EqualTo("classic"))))
	session := access.SecureReadSession(db, policy)
	reader, err := session.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(nil, 2, 0))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextRow(t, reader)
}
