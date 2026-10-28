package dalgo2typesense

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
)

// filterLiteral is one admitted scalar value in the exact shape the compiler
// writes to the native filter. The wire text and the exact comparison value are
// produced together, so a verifier can never compare against a different
// normalization than the one Typesense was asked for. A float32, for example,
// is compared as its shortest float32 wire decimal, never as the extra digits
// of float64(float32).
type filterLiteral struct {
	class   valueClass
	wire    string
	text    string
	boolean bool
	number  *big.Rat
}

func makeFilterLiteral(value any) (filterLiteral, error) {
	switch v := value.(type) {
	case string:
		if v == "" || len(v) > 1024 || !validFilterString(v) {
			return filterLiteral{}, unsafeValue("filter string constant is not supported")
		}
		return filterLiteral{class: classString, wire: "`" + v + "`", text: v}, nil
	case bool:
		if v {
			return filterLiteral{class: classBool, wire: "true", boolean: true}, nil
		}
		return filterLiteral{class: classBool, wire: "false"}, nil
	case int:
		return integerLiteral(strconv.FormatInt(int64(v), 10))
	case int8:
		return integerLiteral(strconv.FormatInt(int64(v), 10))
	case int16:
		return integerLiteral(strconv.FormatInt(int64(v), 10))
	case int32:
		return integerLiteral(strconv.FormatInt(int64(v), 10))
	case int64:
		return integerLiteral(strconv.FormatInt(v, 10))
	case uint:
		return unsignedLiteral(uint64(v))
	case uint8:
		return unsignedLiteral(uint64(v))
	case uint16:
		return unsignedLiteral(uint64(v))
	case uint32:
		return unsignedLiteral(uint64(v))
	case uint64:
		return unsignedLiteral(v)
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return filterLiteral{}, unsafeValue("non-finite numbers are not supported")
		}
		return decimalLiteral(strconv.FormatFloat(float64(v), 'g', -1, 32))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return filterLiteral{}, unsafeValue("non-finite numbers are not supported")
		}
		return decimalLiteral(strconv.FormatFloat(v, 'g', -1, 64))
	case json.Number:
		wire, class, err := formatJSONNumber(v)
		if err != nil {
			return filterLiteral{}, err
		}
		if class != classNumber {
			return filterLiteral{}, unsafeValue("JSON number is not supported")
		}
		return decimalLiteral(wire)
	case time.Time:
		return filterLiteral{}, notSupported("typesense filter timestamps must be numeric application values")
	default:
		return filterLiteral{}, notSupported("typesense comparison values must be JSON scalars")
	}
}

func unsignedLiteral(value uint64) (filterLiteral, error) {
	wire, class, err := formatUint(value)
	if err != nil {
		return filterLiteral{}, err
	}
	if class != classNumber {
		return filterLiteral{}, unsafeValue("unsigned integer is not supported")
	}
	return decimalLiteral(wire)
}

func integerLiteral(wire string) (filterLiteral, error) {
	return decimalLiteral(wire)
}

// decimalLiteral derives the exact comparison value from the wire text. Integer
// wire text stays an exact int64/uint64 rational; a fractional or exponent form
// becomes the exact IEEE754 value of that decimal spelling. No integer is ever
// routed through float64 before representability is decided.
func decimalLiteral(wire string) (filterLiteral, error) {
	literal := filterLiteral{class: classNumber, wire: wire}
	if isIntegerWire(wire) {
		if strings.HasPrefix(wire, "-") {
			value, err := strconv.ParseInt(wire, 10, 64)
			if err != nil {
				return filterLiteral{}, unsafeValue("integer constant is outside int64")
			}
			literal.number = new(big.Rat).SetInt64(value)
			return literal, nil
		}
		value, err := strconv.ParseUint(wire, 10, 64)
		if err != nil {
			return filterLiteral{}, unsafeValue("integer constant is outside uint64")
		}
		literal.number = new(big.Rat).SetUint64(value)
		return literal, nil
	}
	value, err := strconv.ParseFloat(wire, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return filterLiteral{}, unsafeValue("invalid numeric constant")
	}
	literal.number = new(big.Rat).SetFloat64(value)
	return literal, nil
}

func isIntegerWire(wire string) bool {
	if wire == "" {
		return false
	}
	start := 0
	if wire[0] == '-' {
		start = 1
	}
	if start == len(wire) {
		return false
	}
	for i := start; i < len(wire); i++ {
		if wire[i] < '0' || wire[i] > '9' {
			return false
		}
	}
	return true
}

// exactNumberRat converts one decoded native scalar to its exact rational
// value. It is the only numeric comparison primitive: int64/uint64 neighbours
// above 2^53 stay distinct, while a finite float is compared by its exact
// binary value rather than by a formatted string.
func exactNumberRat(value any) (*big.Rat, bool) {
	switch v := value.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(v)), true
	case int8:
		return new(big.Rat).SetInt64(int64(v)), true
	case int16:
		return new(big.Rat).SetInt64(int64(v)), true
	case int32:
		return new(big.Rat).SetInt64(int64(v)), true
	case int64:
		return new(big.Rat).SetInt64(v), true
	case uint:
		return new(big.Rat).SetUint64(uint64(v)), true
	case uint8:
		return new(big.Rat).SetUint64(uint64(v)), true
	case uint16:
		return new(big.Rat).SetUint64(uint64(v)), true
	case uint32:
		return new(big.Rat).SetUint64(uint64(v)), true
	case uint64:
		return new(big.Rat).SetUint64(v), true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(float64(v)), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(v), true
	case json.Number:
		literal, err := makeFilterLiteral(v)
		if err != nil || literal.number == nil {
			return nil, false
		}
		return literal.number, true
	default:
		return nil, false
	}
}

func (l filterLiteral) equalValue(value any) bool {
	switch l.class {
	case classString:
		text, ok := value.(string)
		return ok && text == l.text
	case classBool:
		boolean, ok := value.(bool)
		return ok && boolean == l.boolean
	case classNumber:
		number, ok := exactNumberRat(value)
		return ok && l.number != nil && number.Cmp(l.number) == 0
	default:
		return false
	}
}

func (l filterLiteral) compareNumber(value any) (int, bool) {
	if l.class != classNumber || l.number == nil {
		return 0, false
	}
	number, ok := exactNumberRat(value)
	if !ok {
		return 0, false
	}
	return number.Cmp(l.number), true
}

// observeVerifierContext reports a canonical, detail-free context failure as
// soon as the operation is canceled or its deadline passes. The verifier calls
// it from its inner AND/OR/IN loops and while converting an array value, so a
// cancellation is observed during one row's exact comparison instead of only
// between HTTP pages.
func observeVerifierContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return safeContextError(ctx, err)
	}
	return nil
}

func matchIn(ctx context.Context, fieldValue any, literals []filterLiteral) (bool, error) {
	elements, isArray, err := arrayElements(ctx, fieldValue)
	if err != nil {
		return false, err
	}
	if isArray {
		for _, element := range elements {
			for _, literal := range literals {
				if err := observeVerifierContext(ctx); err != nil {
					return false, err
				}
				if literal.equalValue(element) {
					return true, nil
				}
			}
		}
		return false, nil
	}
	for _, literal := range literals {
		if err := observeVerifierContext(ctx); err != nil {
			return false, err
		}
		if literal.equalValue(fieldValue) {
			return true, nil
		}
	}
	return false, nil
}

// arrayElements eagerly converts one native array value. It observes the
// operation context on every element so a canceled read cannot spend the whole
// bounded array budget before the cancellation is reported.
func arrayElements(ctx context.Context, value any) ([]any, bool, error) {
	if value == nil {
		return nil, false, nil
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		elements := make([]any, reflected.Len())
		for i := 0; i < reflected.Len(); i++ {
			if err := observeVerifierContext(ctx); err != nil {
				return nil, false, err
			}
			elements[i] = reflected.Index(i).Interface()
		}
		return elements, true, nil
	default:
		return nil, false, nil
	}
}

// conditionVerifier is the adapter-local exact evaluator built from the same
// admitted condition AST as the native filter. It supports only the whole
// AND/OR tree and the scalar comparisons the compiler already admits.
type conditionVerifier interface {
	match(ctx context.Context, data map[string]any) (bool, error)
}

type groupVerifier struct {
	operator   dal.Operator
	conditions []conditionVerifier
}

func (g groupVerifier) match(ctx context.Context, data map[string]any) (bool, error) {
	switch g.operator {
	case dal.And:
		for _, condition := range g.conditions {
			if err := observeVerifierContext(ctx); err != nil {
				return false, err
			}
			ok, err := condition.match(ctx, data)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case dal.Or:
		for _, condition := range g.conditions {
			if err := observeVerifierContext(ctx); err != nil {
				return false, err
			}
			ok, err := condition.match(ctx, data)
			if err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	default:
		return false, notSupported("typesense supports AND/OR condition groups only")
	}
}

type comparisonVerifier struct {
	field    string
	operator dal.Operator
	literal  filterLiteral
	literals []filterLiteral
}

func (c comparisonVerifier) match(ctx context.Context, data map[string]any) (bool, error) {
	if err := observeVerifierContext(ctx); err != nil {
		return false, err
	}
	fieldValue, found := data[c.field]
	if !found {
		// A missing or unavailable field can never authorize a row. A true
		// sibling OR branch may still make the whole predicate true.
		return false, nil
	}
	switch c.operator {
	case dal.Equal:
		return c.literal.equalValue(fieldValue), nil
	case dal.In:
		return matchIn(ctx, fieldValue, c.literals)
	case dal.GreaterThen, dal.GreaterOrEqual, dal.LessThen, dal.LessOrEqual:
		comparison, ok := c.literal.compareNumber(fieldValue)
		if !ok {
			return false, nil
		}
		switch c.operator {
		case dal.GreaterThen:
			return comparison > 0, nil
		case dal.GreaterOrEqual:
			return comparison >= 0, nil
		case dal.LessThen:
			return comparison < 0, nil
		default:
			return comparison <= 0, nil
		}
	default:
		return false, notSupported("typesense comparison operator is not supported")
	}
}
