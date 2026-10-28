package dalgo2typesense

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
)

// searchPlan is the bounded, validated shape of one structured DALgo query. It
// is built before any network request, so unsupported joins, grouping, nested
// fields, null tests, operators and value classes cannot reach Typesense.
type searchPlan struct {
	collection    string
	filter        string
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
	pageSize := d.cfg.MaxRowsPerPage
	if pageSize > absoluteMaxRowsPerPage {
		pageSize = absoluteMaxRowsPerPage
	}
	if q.Limit() < pageSize {
		pageSize = q.Limit()
	}
	if requiredPages(q.Limit(), pageSize) > d.cfg.MaxPages {
		return searchPlan{}, notSupported("typesense query needs more pages than MaxPages")
	}

	p := searchPlan{
		collection: d.cfg.Collection,
		limit:      q.Limit(),
		offset:     q.Offset(),
		pageSize:   pageSize,
	}
	var err error
	budget := 0
	if p.filter, err = compileCondition(q.Where(), 0, &budget, d.cfg.MaxFilterBytes); err != nil {
		return searchPlan{}, err
	}
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
	*budget++
	if depth > defaultMaxDepth || *budget > defaultMaxConditions {
		return "", notSupported("typesense condition tree exceeds the configured complexity bound")
	}
	if value == nil {
		return "", nil
	}
	var text string
	switch c := value.(type) {
	case dal.GroupCondition:
		operator := ""
		switch c.Operator() {
		case dal.And:
			operator = " && "
		case dal.Or:
			operator = " || "
		default:
			return "", notSupported("typesense supports AND/OR condition groups only")
		}
		if len(c.Conditions()) == 0 {
			return "", notSupported("typesense empty condition groups are not supported")
		}
		parts := make([]string, 0, len(c.Conditions()))
		for _, child := range c.Conditions() {
			if child == nil {
				return "", notSupported("typesense nil conditions are not supported")
			}
			childText, err := compileCondition(child, depth+1, budget, maxBytes)
			if err != nil {
				return "", err
			}
			if childText == "" {
				return "", notSupported("typesense empty conditions are not supported")
			}
			parts = append(parts, childText)
		}
		text = "(" + strings.Join(parts, operator) + ")"
	case dal.Comparison:
		field, ok := c.Left.(dal.FieldRef)
		if !ok {
			return "", notSupported("typesense comparisons must name a field on the left")
		}
		name, err := structuredField(field)
		if err != nil {
			return "", err
		}
		text, err = compileComparison(name, c.Operator, c.Right)
		if err != nil {
			return "", err
		}
	case dal.IsNullCondition:
		// Typesense has no uniform three-valued NULL semantics for absent and
		// explicit-null values, so both IS NULL and IS NOT NULL are refused
		// before any request rather than silently matching only one class.
		return "", notSupported("typesense NULL tests are not supported")
	default:
		return "", notSupported("typesense condition type is not supported")
	}
	if len(text) > maxBytes {
		return "", ErrRequestTooLarge
	}
	return text, nil
}

func compileComparison(field string, operator dal.Operator, right dal.Expression) (string, error) {
	switch r := right.(type) {
	case dal.Constant:
		if operator == dal.In || operator == dal.NotIn {
			return "", notSupported("typesense IN filters require an array value")
		}
		if r.Value == nil {
			return "", notSupported("typesense NULL comparisons are not supported")
		}
		formatted, class, err := formatFilterValue(r.Value)
		if err != nil {
			return "", err
		}
		switch operator {
		case dal.Equal:
			if class == classString || class == classBool || class == classNumber {
				return field + ":=" + formatted, nil
			}
		case dal.GreaterThen:
			if class == classNumber {
				return field + ":>" + formatted, nil
			}
		case dal.GreaterOrEqual:
			if class == classNumber {
				return field + ":>=" + formatted, nil
			}
		case dal.LessThen:
			if class == classNumber {
				return field + ":<" + formatted, nil
			}
		case dal.LessOrEqual:
			if class == classNumber {
				return field + ":<=" + formatted, nil
			}
		}
		return "", notSupported("typesense comparison operator is not supported for this value class")
	case dal.Array:
		if operator != dal.In && operator != dal.NotIn {
			return "", notSupported("typesense array values require IN or NOT IN")
		}
		values, ok := r.Value.([]any)
		if !ok {
			// dal.Array may hold typed slices; normalize the documented scalar
			// slice classes without reflection-based field access.
			var err error
			values, err = arrayValues(r.Value)
			if err != nil {
				return "", err
			}
		}
		if len(values) == 0 {
			return "", notSupported("typesense empty IN lists are not supported")
		}
		formatted := make([]string, 0, len(values))
		var class valueClass
		for _, value := range values {
			rendered, current, err := formatFilterValue(value)
			if err != nil {
				return "", err
			}
			if class != "" && class != current {
				return "", notSupported("typesense IN lists must use one scalar class")
			}
			class = current
			formatted = append(formatted, rendered)
		}
		operatorText := ":="
		if operator == dal.NotIn {
			operatorText = ":!="
		}
		return field + operatorText + "[" + strings.Join(formatted, ",") + "]", nil
	default:
		return "", notSupported("typesense comparison values must be literals")
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

func formatFilterValue(value any) (string, valueClass, error) {
	switch v := value.(type) {
	case string:
		if v == "" || len(v) > 1024 || !validFilterString(v) {
			return "", "", unsafeValue("filter string constant is not supported")
		}
		return "`" + v + "`", classString, nil
	case bool:
		if v {
			return "true", classBool, nil
		}
		return "false", classBool, nil
	case int:
		return strconv.FormatInt(int64(v), 10), classNumber, nil
	case int8:
		return strconv.FormatInt(int64(v), 10), classNumber, nil
	case int16:
		return strconv.FormatInt(int64(v), 10), classNumber, nil
	case int32:
		return strconv.FormatInt(int64(v), 10), classNumber, nil
	case int64:
		return strconv.FormatInt(v, 10), classNumber, nil
	case uint:
		return formatUint(uint64(v))
	case uint8:
		return formatUint(uint64(v))
	case uint16:
		return formatUint(uint64(v))
	case uint32:
		return formatUint(uint64(v))
	case uint64:
		return formatUint(v)
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return "", "", unsafeValue("non-finite numbers are not supported")
		}
		return strconv.FormatFloat(float64(v), 'g', -1, 32), classNumber, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", "", unsafeValue("non-finite numbers are not supported")
		}
		return strconv.FormatFloat(v, 'g', -1, 64), classNumber, nil
	case json.Number:
		return formatJSONNumber(v)
	case time.Time:
		return "", "", notSupported("typesense filter timestamps must be numeric application values")
	default:
		return "", "", notSupported("typesense comparison values must be JSON scalars")
	}
}

func formatUint(value uint64) (string, valueClass, error) {
	if value > math.MaxInt64 {
		return "", "", unsafeValue("unsigned integer exceeds Typesense int64 range")
	}
	return strconv.FormatUint(value, 10), classNumber, nil
}

func formatJSONNumber(value json.Number) (string, valueClass, error) {
	text := value.String()
	if strings.ContainsAny(text, ".eE") {
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return "", "", unsafeValue("invalid JSON number")
		}
		return strconv.FormatFloat(parsed, 'g', -1, 64), classNumber, nil
	}
	if strings.HasPrefix(text, "-") {
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return "", "", unsafeValue("JSON integer is outside int64")
		}
		return text, classNumber, nil
	}
	parsed, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return "", "", unsafeValue("JSON integer is outside uint64")
	}
	return formatUint(parsed)
}

func validFilterString(value string) bool {
	for _, r := range value {
		if r == '`' || r == 0 || r == '\r' || r == '\n' || r == 0x7f {
			return false
		}
	}
	return true
}
