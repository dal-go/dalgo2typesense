package dalgo2typesense

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

// decodeJSON uses UseNumber so an integer is never routed through float64
// before the adapter validates whether it can be represented losslessly.
func decodeJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return malformed("response is not valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return malformed("response has trailing JSON")
	}
	return nil
}

func requiredArray(raw json.RawMessage, what string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, malformed(what + " is missing a JSON array")
	}
	if trimmed[0] != '[' {
		return nil, malformed(what + " is not a JSON array")
	}
	var values []json.RawMessage
	if err := decodeJSON(trimmed, &values); err != nil {
		return nil, err
	}
	if values == nil {
		return nil, malformed(what + " is not a JSON array")
	}
	return values, nil
}

func requiredObject(raw json.RawMessage, what string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, malformed(what + " is missing a JSON object")
	}
	if trimmed[0] != '{' {
		return nil, malformed(what + " is not a JSON object")
	}
	var value map[string]json.RawMessage
	if err := decodeJSON(trimmed, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, malformed(what + " is not a JSON object")
	}
	return value, nil
}

func requiredInt(raw json.RawMessage, what string) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, malformed(what + " is missing")
	}
	if trimmed[0] == '"' || trimmed[0] == '{' || trimmed[0] == '[' {
		return 0, malformed(what + " is not an integer")
	}
	var number json.Number
	if err := decodeJSON(trimmed, &number); err != nil {
		return 0, malformed(what + " is not an integer")
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, malformed(what + " is not a representable integer")
	}
	if value < 0 || value > math.MaxInt {
		return 0, malformed(what + " is outside the supported range")
	}
	return int(value), nil
}

func optionalInt(raw json.RawMessage, what string) (int, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false, nil
	}
	value, err := requiredInt(trimmed, what)
	if err != nil {
		return 0, false, err
	}
	return value, true, nil
}

func requiredString(raw json.RawMessage, what string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", malformed(what + " is missing")
	}
	var value string
	if err := decodeJSON(trimmed, &value); err != nil {
		return "", malformed(what + " is not a string")
	}
	return value, nil
}

func optionalBool(raw json.RawMessage, what string) (bool, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, false, nil
	}
	var value bool
	if err := decodeJSON(trimmed, &value); err != nil {
		return false, false, malformed(what + " is not a boolean")
	}
	return value, true, nil
}

func optionalFloat(raw json.RawMessage, what string) (float64, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false, nil
	}
	var number json.Number
	if err := decodeJSON(trimmed, &number); err != nil {
		return 0, false, malformed(what + " is not a number")
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false, malformed(what + " is not a finite number")
	}
	return value, true, nil
}

// convertJSONNumbers converts a decoded UseNumber tree into lossless Go
// scalars. Integers that fit int64 become int64; larger non-negative integers
// that fit uint64 become uint64; all other JSON numbers remain float64 after
// explicit finiteness validation. No number is routed through float64 before
// integer representability is decided.
func convertJSONNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		return convertJSONNumber(v)
	case map[string]any:
		for key, child := range v {
			converted, err := convertJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			v[key] = converted
		}
		return v, nil
	case []any:
		for i, child := range v {
			converted, err := convertJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
		return v, nil
	case string, bool, nil, int64, uint64, float64:
		return v, nil
	default:
		return nil, malformed("unsupported JSON value class")
	}
}

func convertJSONNumber(number json.Number) (any, error) {
	text := number.String()
	if text == "" {
		return nil, malformed("empty JSON number")
	}
	if strings.ContainsAny(text, ".eE") {
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, malformed("JSON number is not a finite float64")
		}
		return value, nil
	}
	if strings.HasPrefix(text, "-") {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, malformed("JSON integer is outside int64")
		}
		return value, nil
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return nil, malformed("JSON integer is outside uint64")
	}
	if value <= math.MaxInt64 {
		return int64(value), nil
	}
	return value, nil
}

func encodedSize(value any) (int64, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return int64(len(payload)), nil
}
