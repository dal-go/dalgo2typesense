package dalgo2typesense

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
)

// runSemanticIntegration exercises the exact structured predicate contract
// against the real pinned Typesense 30.2 fixture. The first block records the
// actual native candidate behaviour with raw server queries; the rest proves
// what the adapter does with those real candidates.
func runSemanticIntegration(t *testing.T, f *fixtureEnv, db dal.DB, key string) {
	t.Helper()

	t.Run("numeric-inclusive-candidate-is-actual-superset", func(t *testing.T) {
		strictGreater := f.semanticIDs(t, key, "n:>0.1")
		inclusiveGreater := f.semanticIDs(t, key, "n:>=0.1")
		// The strict native filter really omits stored values that are strictly
		// greater than 0.1. This is the observed provider RED for the numeric
		// contract, not a mocked filter string.
		assertIDSet(t, strictGreater, "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10")
		if containsID(strictGreater, "f1") || containsID(strictGreater, "f2") {
			t.Fatalf("strict > unexpectedly kept the quantised boundary rows: %v", strictGreater)
		}
		// Every strict-eligible raw JSON value must be present in the inclusive
		// candidate set. f1=0.1000000001 and f2=0.10000000149 are the observed
		// omissions; the rest prove the superset beyond the boundary bucket.
		for _, id := range []string{
			"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10",
		} {
			if !containsID(inclusiveGreater, id) {
				t.Fatalf("inclusive >= omitted strict-eligible %s: %v", id, inclusiveGreater)
			}
		}
		if !containsID(inclusiveGreater, "f0") {
			t.Fatalf("inclusive >= did not include the documented boundary false positive f0: %v", inclusiveGreater)
		}

		strictLarge := f.semanticIDs(t, key, "n:>100000000")
		inclusiveLarge := f.semanticIDs(t, key, "n:>=100000000")
		if len(strictLarge) != 0 {
			t.Fatalf("strict >100000000 = %v, want the observed empty result despite stored 100000001", strictLarge)
		}
		assertIDSet(t, inclusiveLarge, "f3", "f4")
		if !containsID(inclusiveLarge, "f3") {
			t.Fatalf("inclusive >=100000000 omitted strict-eligible f3: %v", inclusiveLarge)
		}

		strictLess := f.semanticIDs(t, key, "n:<0.1000000001")
		inclusiveLess := f.semanticIDs(t, key, "n:<=0.1000000001")
		if len(strictLess) != 0 {
			t.Fatalf("strict <0.1000000001 = %v, want the observed empty result despite stored 0.1", strictLess)
		}
		assertIDSet(t, inclusiveLess, "f0", "f1", "f2")
		if !containsID(inclusiveLess, "f0") {
			t.Fatalf("inclusive <=0.1000000001 omitted strict-eligible f0: %v", inclusiveLess)
		}

		strictBelow := f.semanticIDs(t, key, "n:<1.5")
		inclusiveBelow := f.semanticIDs(t, key, "n:<=1.5")
		assertIDSet(t, strictBelow, "f0", "f1", "f2")
		for _, id := range strictBelow {
			if !containsID(inclusiveBelow, id) {
				t.Fatalf("inclusive <=1.5 omitted strict-eligible %s: %v", id, inclusiveBelow)
			}
		}
	})

	t.Run("numeric-strict-large-boundary-refuses-before-first-row", func(t *testing.T) {
		// n > 100000000 is widened to n:>=100000000, whose real candidates are
		// f3 (100000001, eligible) and f4 (100000000, strict boundary). The
		// boundary must refuse the whole read before any row is exposed.
		expectSemanticRefusal(t, db, semanticsQuery(
			numericCondition("n", dal.GreaterThen, 100000000), 10, 0))
	})

	t.Run("numeric-inclusive-operator-keeps-exact-boundary", func(t *testing.T) {
		// >= is unchanged: both f3 and f4 really satisfy the admitted operator.
		ids := semanticQueryIDs(t, db, semanticsQuery(
			numericCondition("n", dal.GreaterOrEqual, 100000000), 10, 0))
		assertIDSet(t, ids, "f3", "f4")
	})

	t.Run("numeric-strict-success-without-boundary", func(t *testing.T) {
		// n > 1 has no n == 1 row, so the widened >=1 candidate contains only
		// strict-eligible rows and the exact query succeeds non-vacuously.
		ids := semanticQueryIDs(t, db, semanticsQuery(
			numericCondition("n", dal.GreaterThen, 1), 10, 0))
		assertIDSet(t, ids, "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10")
	})

	t.Run("integer-exact-boundary-is-safe-superset-or-refuses", func(t *testing.T) {
		// m > 6 uses m:>=6 and includes the exact boundary m=6.
		expectSemanticRefusal(t, db, semanticsQuery(
			numericCondition("m", dal.GreaterThen, 6), 20, 0))
		// m > 5 has no m=5 row, so every included candidate is exact.
		ids := semanticQueryIDs(t, db, semanticsQuery(
			numericCondition("m", dal.GreaterThen, 5), 20, 0))
		assertIDSet(t, ids, "f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10")
		// m < 6 uses m:<=6. f0 (m=4) is exact and comes first, but the later
		// boundary f1 (m=6) must still refuse before f0 is exposed.
		expectSemanticRefusal(t, db, semanticsQuery(
			numericCondition("m", dal.LessThen, 6), 20, 0))
		// <= is unchanged and both m=4 and m=6 really satisfy it.
		ids = semanticQueryIDs(t, db, semanticsQuery(
			numericCondition("m", dal.LessOrEqual, 6), 20, 0))
		assertIDSet(t, ids, "f0", "f1")
	})

	t.Run("integer-fractional-rhs-fails-closed", func(t *testing.T) {
		// Actual 30.2 rejects a fractional comparison on int32 with HTTP 400.
		// That is an explicit failure, never an empty successful result.
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), semanticsQuery(
			numericCondition("m", dal.GreaterThen, 5.5), 5, 0))
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		rec, err := reader.Next()
		if err == nil {
			t.Fatalf("fractional integer RHS returned row %#v", rec.Key().ID)
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("fractional integer RHS err = %v, want *HTTPError 400", err)
		}
		if errors.Is(err, ErrSemanticMismatch) || errors.Is(err, dal.ErrNoMoreRecords) {
			t.Fatalf("fractional integer RHS was not an explicit failure: %v", err)
		}
	})

	t.Run("string-folding-refuses-before-first-row", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			where dal.Condition
		}{
			{name: "case", where: dal.WhereField("label", dal.Equal, "classic")},
			{name: "accent", where: dal.WhereField("label", dal.Equal, "cafe")},
			{name: "punctuation", where: dal.WhereField("label", dal.Equal, "red-blue")},
			{name: "dot-punctuation", where: dal.WhereField("label", dal.Equal, "ab")},
			{name: "whitespace", where: dal.WhereField("label", dal.Equal, "hello world")},
			{name: "string-in", where: dal.WhereField("label", dal.In, []string{"classic"})},
			{name: "array-equal", where: dal.WhereField("tags", dal.Equal, "classic")},
			{name: "array-in-punctuation", where: dal.WhereField("tags", dal.In, []string{"a_b"})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				expectSemanticRefusal(t, db, semanticsQuery(tc.where, 20, 0))
			})
		}
	})

	t.Run("intentional-array-in-succeeds", func(t *testing.T) {
		ids := semanticQueryIDs(t, db, semanticsQuery(
			dal.WhereField("tags", dal.In, []string{"desert"}), 20, 0))
		assertIDSet(t, ids, "f0")
		ids = semanticQueryIDs(t, db, semanticsQuery(
			dal.WhereField("tags", dal.In, []string{"sequel"}), 20, 0))
		assertIDSet(t, ids, "f1")
	})

	t.Run("whole-and-or-semantics", func(t *testing.T) {
		// A missing note field cannot authorize itself, but the true m=7
		// sibling branch makes the whole OR true for f2.
		orCondition := dal.NewGroupCondition(dal.Or,
			dal.WhereField("note", dal.Equal, "never"),
			dal.WhereField("m", dal.Equal, 7))
		ids := semanticQueryIDs(t, db, semanticsQuery(orCondition, 20, 0))
		assertIDSet(t, ids, "f2")

		// A real AND refinement: the native candidate is already narrowed to
		// f4 and both exact branches hold.
		andCondition := dal.NewGroupCondition(dal.And,
			dal.WhereField("label", dal.Equal, "redblue"),
			dal.WhereField("m", dal.Equal, 10))
		ids = semanticQueryIDs(t, db, semanticsQuery(andCondition, 20, 0))
		assertIDSet(t, ids, "f4")

		// A whole OR candidate that fails every exact branch refuses instead of
		// exposing the exact f0 prefix.
		expectSemanticRefusal(t, db, semanticsQuery(dal.NewGroupCondition(dal.Or,
			dal.WhereField("label", dal.Equal, "classic"),
			dal.WhereField("m", dal.Equal, 7)), 20, 0))
	})

	t.Run("secure-read-session-residual-refuses", func(t *testing.T) {
		policy := access.MustPolicy("semantics-rows",
			access.Collection("semantics", access.Allow(access.Query, "rows").Where(
				numericCondition("n", dal.GreaterThen, json.Number("0.1")))))
		session := access.SecureReadSession(db, policy)
		reader, err := session.ExecuteQueryToRecordsReader(context.Background(),
			semanticsQuery(nil, 10, 0))
		if err != nil {
			t.Fatalf("secure reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		rec, err := reader.Next()
		if err == nil {
			t.Fatalf("policy residual candidate leaked row %#v", rec.Key().ID)
		}
		if !errors.Is(err, ErrSemanticMismatch) || !errors.Is(err, dal.ErrNotSupported) {
			t.Fatalf("policy residual err = %v, want ErrSemanticMismatch and dal.ErrNotSupported", err)
		}
	})

	t.Run("filtered-offset-validates-prefix", func(t *testing.T) {
		// The skipped prefix is m=6,7; both are exact, so offset 2 correctly
		// starts at m=9.
		ids := semanticQueryIDs(t, db, semanticsQuery(
			numericCondition("m", dal.GreaterThen, 5), 2, 2))
		if len(ids) != 2 || ids[0] != "f3" || ids[1] != "f4" {
			t.Fatalf("filtered offset ids = %v, want [f3 f4]", ids)
		}
		// An exact first row cannot hide a mismatch inside the skipped prefix.
		expectSemanticRefusal(t, db, semanticsQuery(
			dal.WhereField("label", dal.Equal, "classic"), 1, 1))
	})

	t.Run("later-page-mismatch-has-no-prefix", func(t *testing.T) {
		smallDB := f.dbWithConfig(t, "semantics", key, func(cfg *Config) {
			cfg.MaxRowsPerPage = 1
		})
		expectSemanticRefusal(t, smallDB, semanticsQuery(
			dal.WhereField("label", dal.Equal, "classic"), 3, 0))
	})

	t.Run("projection-strips-predicate-helpers", func(t *testing.T) {
		query := semanticsQuery(dal.WhereField("m", dal.GreaterOrEqual, 6), 1, 0, "label")
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		rec, err := reader.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		data, ok := rec.Data().(map[string]any)
		if !ok {
			t.Fatalf("row data = %T", rec.Data())
		}
		if len(data) != 1 || data["label"] != "Classic" {
			t.Fatalf("projected row = %#v, want only label", data)
		}
		for _, leaked := range []string{"id", "m", "note"} {
			if _, ok := data[leaked]; ok {
				t.Fatalf("predicate helper %q leaked into projected row %#v", leaked, data)
			}
		}
	})

	t.Run("native-search-keeps-native-semantics", func(t *testing.T) {
		searcher, _ := dal.As[NativeSearcher](db)
		result, err := searcher.Search(context.Background(), SearchRequest{
			Q: "*", FilterBy: "label:=`classic`", Limit: 20,
		})
		if err != nil {
			t.Fatalf("native casefold search: %v", err)
		}
		assertIDSet(t, hitIDs(result.Hits), "f0", "f1", "f2")

		result, err = searcher.Search(context.Background(), SearchRequest{
			Q: "*", FilterBy: "label:=`cafe`", Limit: 20,
		})
		if err != nil {
			t.Fatalf("native accent search: %v", err)
		}
		assertIDSet(t, hitIDs(result.Hits), "f5", "f6")

		// Native negative filters stay native and are not the structured
		// NotIn path, which refuses before I/O.
		result, err = searcher.Search(context.Background(), SearchRequest{
			Q: "*", FilterBy: "label:!=`classic`", Limit: 20,
		})
		if err != nil {
			t.Fatalf("native negative search: %v", err)
		}
		assertIDSet(t, hitIDs(result.Hits), "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10")
	})
}

// semanticsQuery builds a deterministic structured query over the semantics
// collection. m is unique, so ordering by it makes paging and offset assertions
// repeatable while still exercising real server ordering.
func semanticsQuery(where dal.Condition, limit, offset int, columns ...string) dal.Query {
	var q dal.IQueryBuilder = dal.From(dal.NewRootCollectionRef("semantics", "")).NewQuery()
	if where != nil {
		q = q.Where(where)
	}
	q = q.OrderBy(dal.AscendingField("m")).Limit(limit).Offset(offset)
	if len(columns) > 0 {
		selected := make([]dal.Column, 0, len(columns))
		for _, name := range columns {
			selected = append(selected, dal.Column{Expression: dal.Field(name)})
		}
		return q.SelectColumns(selected...)
	}
	return q.SelectIntoRecord(nil)
}

func (f *fixtureEnv) semanticIDs(t *testing.T, key, filter string) []string {
	t.Helper()
	params := url.Values{}
	params.Set("q", "*")
	params.Set("filter_by", filter)
	params.Set("per_page", "50")
	params.Set("sort_by", "m:asc")
	status, body := f.do(t, key, http.MethodGet,
		"/collections/semantics/documents/search?"+params.Encode(), "")
	if status != http.StatusOK {
		t.Fatalf("raw semantics search %q: HTTP %d, %s", filter, status, body)
	}
	var decoded struct {
		Found int `json:"found"`
		Hits  []struct {
			Document struct {
				ID string `json:"id"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("raw semantics search %q decode: %v", filter, err)
	}
	if decoded.Found != len(decoded.Hits) {
		t.Fatalf("raw semantics search %q found=%d but received %d hits", filter, decoded.Found, len(decoded.Hits))
	}
	ids := make([]string, 0, len(decoded.Hits))
	for _, hit := range decoded.Hits {
		ids = append(ids, hit.Document.ID)
	}
	return ids
}

func semanticQueryIDs(t *testing.T, db dal.DB, query dal.Query) []string {
	t.Helper()
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	ids := make([]string, 0, 4)
	for {
		rec, nextErr := reader.Next()
		if errors.Is(nextErr, dal.ErrNoMoreRecords) {
			break
		}
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
		id, ok := rec.Key().ID.(string)
		if !ok {
			t.Fatalf("id %#v is not a string", rec.Key().ID)
		}
		ids = append(ids, id)
	}
	return ids
}

func expectSemanticRefusal(t *testing.T, db dal.DB, query dal.Query) {
	t.Helper()
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	rec, err := reader.Next()
	if err == nil {
		t.Fatalf("semantic mismatch exposed row %#v", rec.Key().ID)
	}
	if !errors.Is(err, ErrSemanticMismatch) || !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("Next err = %v, want ErrSemanticMismatch and dal.ErrNotSupported", err)
	}
	// The refusal is terminal and stable: a retry cannot turn it into a prefix
	// or a clean EOF.
	if _, again := reader.Next(); !errors.Is(again, err) {
		t.Fatalf("second Next err = %v, want the same refusal %v", again, err)
	}
}
