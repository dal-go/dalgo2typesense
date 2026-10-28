package dalgo2typesense

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/dal"
)

// searchPlan is the bounded, validated shape of one structured DALgo query. It
// is built before any network request, so unsupported joins, grouping, nested
// fields, null tests, operators and value classes cannot reach Typesense.
type searchPlan struct {
	collection    string
	filter        string
	verifier      conditionVerifier
	predicate     []string
	filtered      bool
	window        int
	sort          []SortField
	columns       []string
	fetch         []string
	hasProjection bool
	limit         int
	offset        int
	pageSize      int
}

func (d *backend) makePlan(query dal.Query) (searchPlan, error) {
	if query == nil {
		return searchPlan{}, notSupported("typesense requires a structured query")
	}
	q, ok := query.(dal.StructuredQuery)
	if !ok || q.From() == nil || q.From().Base() == nil {
		return searchPlan{}, notSupported("typesense supports structured single-collection queries only")
	}
	if len(q.From().Joins()) != 0 {
		return searchPlan{}, notSupported("typesense joins are not supported")
	}
	if len(q.GroupBy()) != 0 || q.Having() != nil {
		return searchPlan{}, notSupported("typesense grouping, having and aggregation are not supported")
	}
	if q.StartFrom() != "" || q.StartAfter() != "" {
		return searchPlan{}, notSupported("typesense DALgo query cursors are not supported")
	}
	if q.IDKind() != reflect.Invalid {
		return searchPlan{}, notSupported("typesense keys-only queries are not supported")
	}
	ref, ok := q.From().Base().(dal.CollectionRef)
	if !ok {
		return searchPlan{}, notSupported("typesense requires one root collection reference")
	}
	if err := d.validateRef(&ref); err != nil {
		return searchPlan{}, err
	}
	if q.Limit() <= 0 {
		return searchPlan{}, notSupported("typesense requires a positive bounded limit")
	}
	if q.Limit() > d.cfg.MaxRows {
		return searchPlan{}, notSupported("typesense query limit exceeds MaxRows")
	}
	if q.Offset() < 0 || q.Offset() > d.cfg.MaxOffset {
		return searchPlan{}, notSupported("typesense query offset is outside the configured bound")
	}
	p := searchPlan{
		collection: d.cfg.Collection,
		limit:      q.Limit(),
		offset:     q.Offset(),
	}
	var err error
	budget := 0
	predicateFields := map[string]bool{}
	if p.filter, p.verifier, err = compileConditionWithVerifier(
		q.Where(), 0, &budget, d.cfg.MaxFilterBytes, predicateFields); err != nil {
		return searchPlan{}, err
	}
	p.filtered = p.filter != ""
	if p.filtered && p.verifier == nil {
		return searchPlan{}, notSupported("typesense structured filter has no exact verifier")
	}
	for name := range predicateFields {
		p.predicate = append(p.predicate, name)
	}
	sort.Strings(p.predicate)
	if len(p.predicate) > d.cfg.MaxColumns {
		return searchPlan{}, notSupported("typesense predicate helper fields exceed MaxColumns")
	}

	pageSize := d.cfg.MaxRowsPerPage
	if pageSize > absoluteMaxRowsPerPage {
		pageSize = absoluteMaxRowsPerPage
	}
	if p.filtered {
		// A filtered request must validate every candidate before the requested
		// offset, so its physical window is offset+limit and starts at zero.
		p.window = q.Offset() + q.Limit()
		if p.window < pageSize {
			pageSize = p.window
		}
		if requiredPages(p.window, pageSize) > d.cfg.MaxPages {
			return searchPlan{}, notSupported("typesense filtered candidate window needs more pages than MaxPages")
		}
	} else {
		if q.Limit() < pageSize {
			pageSize = q.Limit()
		}
		if requiredPages(q.Limit(), pageSize) > d.cfg.MaxPages {
			return searchPlan{}, notSupported("typesense query needs more pages than MaxPages")
		}
	}
	p.pageSize = pageSize

	for _, order := range q.OrderBy() {
		if order == nil {
			return searchPlan{}, notSupported("typesense order expressions must name simple fields")
		}
		field, ok := order.Expression().(dal.FieldRef)
		if !ok || field.Source() != "" {
			return searchPlan{}, notSupported("typesense order expressions must name simple fields")
		}
		name, err := structuredField(field)
		if err != nil {
			return searchPlan{}, err
		}
		if name == "id" {
			return searchPlan{}, notSupported("typesense does not invent an id sort")
		}
		for _, existing := range p.sort {
			if existing.Field == name {
				return searchPlan{}, notSupported("typesense duplicate order fields are not supported")
			}
		}
		if len(p.sort) >= d.cfg.MaxSortFields || len(p.sort) >= absoluteMaxSortFields {
			return searchPlan{}, notSupported("typesense sort_by exceeds the configured bound")
		}
		p.sort = append(p.sort, SortField{Field: name, Descending: order.Descending()})
	}

	seen := map[string]bool{}
	for _, column := range q.Columns() {
		if column.Wildcard != nil {
			return searchPlan{}, notSupported("typesense wildcard projections are not supported")
		}
		field, ok := column.Expression.(dal.FieldRef)
		if !ok {
			return searchPlan{}, notSupported("typesense projections must name simple fields")
		}
		name, err := structuredField(field)
		if err != nil {
			return searchPlan{}, err
		}
		if column.Alias != "" && column.Alias != field.Name() {
			return searchPlan{}, notSupported("typesense projection aliases are not supported")
		}
		if seen[name] {
			return searchPlan{}, notSupported("typesense duplicate projection fields are not supported")
		}
		if len(p.columns) >= d.cfg.MaxColumns {
			return searchPlan{}, notSupported("typesense projection exceeds MaxColumns")
		}
		seen[name] = true
		p.columns = append(p.columns, name)
	}
	if len(p.columns) > 0 {
		p.hasProjection = true
		p.fetch = append(p.fetch, p.columns...)
		if !seen["id"] {
			p.fetch = append([]string{"id"}, p.fetch...)
		}
		// Predicate helper fields are fetched with the declared projection so
		// the exact verifier can see them. They count against the same
		// MaxColumns bound and are stripped before output construction.
		fetched := make(map[string]bool, len(p.fetch))
		for _, name := range p.fetch {
			fetched[name] = true
		}
		helpers := 0
		for _, name := range p.predicate {
			if fetched[name] {
				continue
			}
			fetched[name] = true
			p.fetch = append(p.fetch, name)
			helpers++
		}
		if len(p.columns)+helpers > d.cfg.MaxColumns {
			return searchPlan{}, notSupported("typesense predicate helper fields exceed MaxColumns")
		}
	}
	return p, nil
}

func requiredPages(limit, pageSize int) int {
	if pageSize <= 0 {
		return 0
	}
	return (limit + pageSize - 1) / pageSize
}

func structuredField(field dal.FieldRef) (string, error) {
	if field.Source() != "" {
		return "", notSupported("typesense qualified fields are not supported")
	}
	if field.IsID() || field.Name() == "id" {
		return "id", nil
	}
	if !validateFieldName(field.Name(), false) {
		return "", notSupported("typesense supports simple top-level application fields only")
	}
	return field.Name(), nil
}

func compileCondition(value dal.Condition, depth int, budget *int, maxBytes int) (string, error) {
	text, _, err := compileConditionWithVerifier(value, depth, budget, maxBytes, nil)
	return text, err
}

// compileConditionWithVerifier compiles the one admitted condition AST into a
// native candidate filter and the exact verifier that decides whether a
// candidate really satisfies the same tree. The two outputs are produced in
// one traversal, so they cannot drift apart. Unsupported operators, value
// classes and shapes fail here, before any request.
func compileConditionWithVerifier(value dal.Condition, depth int, budget *int, maxBytes int, fields map[string]bool) (string, conditionVerifier, error) {
	*budget++
	if depth > defaultMaxDepth || *budget > defaultMaxConditions {
		return "", nil, notSupported("typesense condition tree exceeds the configured complexity bound")
	}
	if value == nil {
		return "", nil, nil
	}
	var text string
	var verifier conditionVerifier
	switch c := value.(type) {
	case dal.GroupCondition:
		operator := ""
		switch c.Operator() {
		case dal.And:
			operator = " && "
		case dal.Or:
			operator = " || "
		default:
			return "", nil, notSupported("typesense supports AND/OR condition groups only")
		}
		if len(c.Conditions()) == 0 {
			return "", nil, notSupported("typesense empty condition groups are not supported")
		}
		parts := make([]string, 0, len(c.Conditions()))
		children := make([]conditionVerifier, 0, len(c.Conditions()))
		for _, child := range c.Conditions() {
			if child == nil {
				return "", nil, notSupported("typesense nil conditions are not supported")
			}
			childText, childVerifier, err := compileConditionWithVerifier(
				child, depth+1, budget, maxBytes, fields)
			if err != nil {
				return "", nil, err
			}
			if childText == "" || childVerifier == nil {
				return "", nil, notSupported("typesense empty conditions are not supported")
			}
			parts = append(parts, childText)
			children = append(children, childVerifier)
		}
		text = "(" + strings.Join(parts, operator) + ")"
		verifier = groupVerifier{operator: c.Operator(), conditions: children}
	case dal.Comparison:
		field, ok := c.Left.(dal.FieldRef)
		if !ok {
			return "", nil, notSupported("typesense comparisons must name a field on the left")
		}
		name, err := structuredField(field)
		if err != nil {
			return "", nil, err
		}
		if fields != nil {
			fields[name] = true
		}
		text, verifier, err = compileComparison(name, c.Operator, c.Right)
		if err != nil {
			return "", nil, err
		}
	case dal.IsNullCondition:
		// Typesense has no uniform three-valued NULL semantics for absent and
		// explicit-null values, so both IS NULL and IS NOT NULL are refused
		// before any request rather than silently matching only one class.
		return "", nil, notSupported("typesense NULL tests are not supported")
	default:
		return "", nil, notSupported("typesense condition type is not supported")
	}
	if len(text) > maxBytes {
		return "", nil, ErrRequestTooLarge
	}
	return text, verifier, nil
}

func compileComparison(field string, operator dal.Operator, right dal.Expression) (string, conditionVerifier, error) {
	if operator == dal.NotIn {
		// Native negative filters are not a safe superset for exact strings:
		// normalization can omit a row that the admitted semantics would keep.
		// The explicit native search path retains its negative filter grammar.
		return "", nil, notSupported("typesense structured NOT IN is not supported")
	}
	switch r := right.(type) {
	case dal.Constant:
		if operator == dal.In {
			return "", nil, notSupported("typesense IN filters require an array value")
		}
		if r.Value == nil {
			return "", nil, notSupported("typesense NULL comparisons are not supported")
		}
		literal, err := makeFilterLiteral(r.Value)
		if err != nil {
			return "", nil, err
		}
		verifier := comparisonVerifier{field: field, operator: operator, literal: literal}
		switch operator {
		case dal.Equal:
			if literal.class == classString || literal.class == classBool || literal.class == classNumber {
				return field + ":=" + literal.wire, verifier, nil
			}
		case dal.GreaterThen:
			if literal.class == classNumber {
				// A strict native > can omit strict-eligible raw values because
				// a FLOAT index quantizes neighbouring decimals: for example
				// n:>0.1 omitted the stored values 0.1000000001 and
				// 0.10000000149 in the pinned 30.2 probe. The inclusive >=
				// candidate is a documented superset. The verifier still
				// evaluates the original > operator, so a boundary candidate is
				// refused instead of being silently dropped.
				return field + ":>=" + literal.wire, verifier, nil
			}
		case dal.GreaterOrEqual:
			if literal.class == classNumber {
				return field + ":>=" + literal.wire, verifier, nil
			}
		case dal.LessThen:
			if literal.class == classNumber {
				// Symmetric to >: a strict native < can omit strict-eligible
				// values on the quantized boundary, so the candidate is <=.
				// The verifier keeps the original < operator.
				return field + ":<=" + literal.wire, verifier, nil
			}
		case dal.LessOrEqual:
			if literal.class == classNumber {
				return field + ":<=" + literal.wire, verifier, nil
			}
		}
		return "", nil, notSupported("typesense comparison operator is not supported for this value class")
	case dal.Array:
		if operator != dal.In {
			return "", nil, notSupported("typesense array values require IN")
		}
		values, ok := r.Value.([]any)
		if !ok {
			// dal.Array may hold typed slices; normalize the documented scalar
			// slice classes without reflection-based field access.
			var err error
			values, err = arrayValues(r.Value)
			if err != nil {
				return "", nil, err
			}
		}
		if len(values) == 0 {
			return "", nil, notSupported("typesense empty IN lists are not supported")
		}
		formatted := make([]string, 0, len(values))
		literals := make([]filterLiteral, 0, len(values))
		var class valueClass
		for _, value := range values {
			literal, err := makeFilterLiteral(value)
			if err != nil {
				return "", nil, err
			}
			if class != "" && class != literal.class {
				return "", nil, notSupported("typesense IN lists must use one scalar class")
			}
			class = literal.class
			formatted = append(formatted, literal.wire)
			literals = append(literals, literal)
		}
		return field + ":=" + "[" + strings.Join(formatted, ",") + "]",
			comparisonVerifier{field: field, operator: dal.In, literals: literals}, nil
	default:
		return "", nil, notSupported("typesense comparison values must be literals")
	}
}

func arrayValues(value any) ([]any, error) {
	switch values := value.(type) {
	case []string:
		result := make([]any, len(values))
		for i, v := range values {
			result[i] = v
		}
		return result, nil
	case []int:
		return intsToAny(values), nil
	case []int8:
		return intsToAny(values), nil
	case []int16:
		return intsToAny(values), nil
	case []int32:
		return intsToAny(values), nil
	case []int64:
		return intsToAny(values), nil
	case []uint:
		return uintsToAny(values), nil
	case []uint8:
		return uintsToAny(values), nil
	case []uint16:
		return uintsToAny(values), nil
	case []uint32:
		return uintsToAny(values), nil
	case []uint64:
		return uintsToAny(values), nil
	case []float32:
		return floatsToAny(values), nil
	case []float64:
		return floatsToAny(values), nil
	case []bool:
		result := make([]any, len(values))
		for i, v := range values {
			result[i] = v
		}
		return result, nil
	default:
		return nil, notSupported("typesense IN lists require a scalar array")
	}
}

func intsToAny[T ~int | ~int8 | ~int16 | ~int32 | ~int64](values []T) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

func uintsToAny[T ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64](values []T) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

func floatsToAny[T ~float32 | ~float64](values []T) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

type valueClass string

const (
	classString valueClass = "string"
	classNumber valueClass = "number"
	classBool   valueClass = "bool"
)

func formatUint(value uint64) (string, valueClass, error) {
	if value > math.MaxInt64 {
		return "", "", unsafeValue("unsigned integer exceeds Typesense int64 range")
	}
	return strconv.FormatUint(value, 10), classNumber, nil
}

func formatJSONNumber(value json.Number) (string, valueClass, error) {
	exact, integral, err := exactDecimalInteger(value.String())
	if err != nil {
		return "", "", unsafeValue("JSON number is outside the supported range")
	}
	if integral {
		switch number := exact.(type) {
		case int64:
			return strconv.FormatInt(number, 10), classNumber, nil
		case uint64:
			return formatUint(number)
		}
		return "", "", unsafeValue("JSON number is not supported")
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return "", "", unsafeValue("invalid JSON number")
	}
	return strconv.FormatFloat(parsed, 'g', -1, 64), classNumber, nil
}

func validFilterString(value string) bool {
	for _, r := range value {
		if r == '`' || r == 0 || r == '\r' || r == '\n' || r == 0x7f {
			return false
		}
	}
	return true
}
