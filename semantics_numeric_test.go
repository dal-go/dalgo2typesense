package dalgo2typesense

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// numericCondition builds a comparison directly because dal.WhereField does
// not admit json.Number.
func numericCondition(name string, operator dal.Operator, value any) dal.Condition {
	return dal.Comparison{Left: dal.Field(name), Operator: operator, Right: dal.Constant{Value: value}}
}

// TestNumericCandidate_StrictOperatorsLowerToInclusive pins the approved
// numeric candidate lowering. The native filter is a safe superset: a strict
// > / < becomes the inclusive boundary candidate, while >= / <= are unchanged.
func TestNumericCandidate_StrictOperatorsLowerToInclusive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator dal.Operator
		value    any
		want     string
	}{
		{name: "gt-int", operator: dal.GreaterThen, value: 10, want: "n:>=10"},
		{name: "ge-int", operator: dal.GreaterOrEqual, value: 10, want: "n:>=10"},
		{name: "lt-int", operator: dal.LessThen, value: 10, want: "n:<=10"},
		{name: "le-int", operator: dal.LessOrEqual, value: 10, want: "n:<=10"},
		{name: "gt-float32-wire", operator: dal.GreaterThen, value: float32(0.1), want: "n:>=0.1"},
		{name: "lt-decimal", operator: dal.LessThen, value: json.Number("0.1000000001"), want: "n:<=0.1000000001"},
		{name: "gt-large-adjacent", operator: dal.GreaterThen, value: json.Number("9007199254740993"), want: "n:>=9007199254740993"},
		{name: "ge-negative", operator: dal.GreaterOrEqual, value: int64(-7), want: "n:>=-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := compileCondition(numericCondition("n", tc.operator, tc.value), 0, new(int), 1024)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if text != tc.want {
				t.Fatalf("candidate = %q, want %q", text, tc.want)
			}
		})
	}
}

// TestNumericVerifier_KeepsOriginalStrictOperator proves the widened candidate
// does not widen the semantic result: the verifier still applies > and <.
func TestNumericVerifier_KeepsOriginalStrictOperator(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator dal.Operator
		right    any
		value    any
		want     bool
	}{
		{name: "gt-boundary-false", operator: dal.GreaterThen, right: json.Number("0.1"), value: float64(0.1), want: false},
		{name: "gt-next-float-true", operator: dal.GreaterThen, right: json.Number("0.1"), value: float64(0.1000000001), want: true},
		{name: "gt-int-boundary-false", operator: dal.GreaterThen, right: int64(1965), value: int64(1965), want: false},
		{name: "gt-int-above-true", operator: dal.GreaterThen, right: int64(1965), value: int64(1969), want: true},
		{name: "lt-boundary-false", operator: dal.LessThen, right: json.Number("0.1000000001"), value: float64(0.1000000001), want: false},
		{name: "lt-below-true", operator: dal.LessThen, right: json.Number("0.1000000001"), value: float64(0.1), want: true},
		{name: "ge-boundary-true", operator: dal.GreaterOrEqual, right: int64(1965), value: int64(1965), want: true},
		{name: "le-boundary-true", operator: dal.LessOrEqual, right: int64(1965), value: int64(1965), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, verifier, err := compileConditionWithVerifier(
				numericCondition("n", tc.operator, tc.right), 0, new(int), 1024, nil)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := verifier.match(context.Background(), map[string]any{"n": tc.value})
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNumericLiteral_Float32UsesWireDecimal pins that a float32 RHS is compared
// as the compiler's shortest float32 wire decimal, never as the extra binary64
// digits of float64(float32(v)).
func TestNumericLiteral_Float32UsesWireDecimal(t *testing.T) {
	literal, err := makeFilterLiteral(float32(0.1))
	if err != nil {
		t.Fatalf("makeFilterLiteral: %v", err)
	}
	if literal.wire != "0.1" {
		t.Fatalf("wire = %q, want 0.1", literal.wire)
	}
	rawCast := strconv.FormatFloat(float64(float32(0.1)), 'g', -1, 64)
	if rawCast == "0.1" {
		t.Fatalf("test premise broken: raw float64(float32(0.1)) also formats as 0.1")
	}
	if !literal.equalValue(float64(0.1)) {
		t.Fatalf("wire canonical 0.1 did not compare equal to JSON-decoded 0.1")
	}
	if literal.equalValue(float64(float32(0.1))) {
		t.Fatalf("float32 binary expansion was treated as the decimal 0.1 wire value")
	}
}

// TestNumericLiteral_AdjacentLargeIntegers proves int64/uint64 neighbours above
// 2^53 stay distinct and are never routed through float64.
func TestNumericLiteral_AdjacentLargeIntegers(t *testing.T) {
	literal, err := makeFilterLiteral(json.Number("9007199254740993"))
	if err != nil {
		t.Fatalf("makeFilterLiteral: %v", err)
	}
	for _, tc := range []struct {
		name  string
		value any
		want  int
	}{
		{name: "below-int64", value: int64(9007199254740992), want: -1},
		{name: "equal-int64", value: int64(9007199254740993), want: 0},
		{name: "above-uint64", value: uint64(9007199254740994), want: 1},
		{name: "equal-json-number", value: json.Number("9007199254740993"), want: 0},
		{name: "float64-neighbour-is-not-equal", value: float64(9007199254740992), want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := literal.compareNumber(tc.value)
			if !ok {
				t.Fatalf("compareNumber(%#v) was not a number", tc.value)
			}
			if got != tc.want {
				t.Fatalf("compare = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNumericLiteral_CrossNumericClasses pins deliberate exact comparison
// across int, uint, float and json.Number spellings.
func TestNumericLiteral_CrossNumericClasses(t *testing.T) {
	ten, err := makeFilterLiteral(int64(10))
	if err != nil {
		t.Fatalf("makeFilterLiteral(10): %v", err)
	}
	for _, value := range []any{
		int64(10), int(10), uint64(10), float64(10), json.Number("10"), json.Number("1e1"), json.Number("10.0"),
	} {
		if !ten.equalValue(value) {
			t.Fatalf("exact 10 did not equal %#v (%T)", value, value)
		}
	}
	for _, value := range []any{int64(11), uint64(9), float64(10.5), json.Number("1.0000001e1")} {
		if ten.equalValue(value) {
			t.Fatalf("exact 10 wrongly equal to %#v (%T)", value, value)
		}
	}
	hundred, err := makeFilterLiteral(json.Number("1e2"))
	if err != nil {
		t.Fatalf("makeFilterLiteral(1e2): %v", err)
	}
	if !hundred.equalValue(int64(100)) || !hundred.equalValue(uint64(100)) {
		t.Fatalf("integral exponent 1e2 did not stay exactly 100")
	}
	if hundred.wire != "100" {
		t.Fatalf("1e2 wire = %q, want 100", hundred.wire)
	}
}

// TestNumericCandidate_IntegerFractionalRHS is the integer-field refinement:
// a fractional RHS must be a safe superset of the strict interpretation or the
// read must fail closed. A row exactly on the RHS refuses; a row beyond it
// succeeds.
func TestNumericCandidate_IntegerFractionalRHS(t *testing.T) {
	t.Run("fractional-rhs-succeeds-beyond-boundary", func(t *testing.T) {
		var filters []string
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			filters = append(filters, r.URL.Query().Get("filter_by"))
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"a","m":6}}]}`)
		})
		db := newTestDB(t, server, nil)
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
			structuredBooksQuery(dal.WhereField("m", dal.GreaterThen, 5.5), 1, 0))
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
		if len(filters) != 1 || filters[0] != "m:>=5.5" {
			t.Fatalf("filter_by = %v, want [m:>=5.5]", filters)
		}
	})

	t.Run("exact-integer-boundary-refuses", func(t *testing.T) {
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"found":2,"out_of":2,"page":1,"hits":[
				{"document":{"id":"a","m":5}},
				{"document":{"id":"b","m":6}}]}`)
		})
		db := newTestDB(t, server, nil)
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
			structuredBooksQuery(dal.WhereField("m", dal.GreaterThen, 5), 2, 0))
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		nextRow(t, reader)
	})

	t.Run("truncated-integer-candidate-refuses", func(t *testing.T) {
		// If a provider treated >=5.5 as >=5, the boundary row is a false
		// positive and must refuse instead of producing an empty or partial
		// success.
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
				{"document":{"id":"a","m":5}}]}`)
		})
		db := newTestDB(t, server, nil)
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
			structuredBooksQuery(dal.WhereField("m", dal.GreaterThen, 5.5), 1, 0))
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		nextRow(t, reader)
	})
}

// TestNumericCandidate_WidenedBoundaryHasNoPrefix proves an inclusive candidate
// boundary row cannot leak an earlier apparently successful row, even when it is
// returned on a later page.
func TestNumericCandidate_WidenedBoundaryHasNoPrefix(t *testing.T) {
	var requests int
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Query().Get("offset") {
		case "":
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":3,"page":1,"hits":[
				{"document":{"id":"a","n":0.1000000001}}]}`)
		case "1":
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":3,"page":1,"hits":[
				{"document":{"id":"b","n":1.5}}]}`)
		case "2":
			writeJSON(t, w, http.StatusOK, `{"found":3,"out_of":3,"page":1,"hits":[
				{"document":{"id":"c","n":0.1}}]}`)
		default:
			t.Errorf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
	})
	db := newTestDB(t, server, func(cfg *Config) { cfg.MaxRowsPerPage = 1 })
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(numericCondition("n", dal.GreaterThen, json.Number("0.1")), 3, 0))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextRow(t, reader)
	if requests != 3 {
		t.Fatalf("requests = %d, want all 3 candidates validated before exposure", requests)
	}
}

// TestNumericCandidate_StrictSuccessWithoutBoundary verifies a strict query is
// still a successful non-empty exact result when the widened candidate window
// happens to contain no boundary false positive.
func TestNumericCandidate_StrictSuccessWithoutBoundary(t *testing.T) {
	var filters []string
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		filters = append(filters, r.URL.Query().Get("filter_by"))
		writeJSON(t, w, http.StatusOK, `{"found":2,"out_of":2,"page":1,"hits":[
			{"document":{"id":"a","n":0.1000000001}},
			{"document":{"id":"b","n":1.5}}]}`)
	})
	db := newTestDB(t, server, nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		structuredBooksQuery(dal.WhereField("n", dal.GreaterThen, 0.1), 2, 0))
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
	if len(filters) != 1 || filters[0] != "n:>=0.1" {
		t.Fatalf("filter_by = %v, want [n:>=0.1]", filters)
	}
}
