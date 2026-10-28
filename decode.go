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

func optionalNumber(raw json.RawMessage, what string) (any, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false, nil
	}
	var number json.Number
	if err := decodeJSON(trimmed, &number); err != nil {
		return nil, false, malformed(what + " is not a number")
	}
	value, err := convertJSONNumber(number)
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// convertJSONNumbers converts a decoded UseNumber tree into Go scalars that
// keep the exact value. Integral JSON numbers become int64 or uint64 even when
// they are written with an exponent or a decimal point. Only non-integral
// numbers become finite float64 values with documented IEEE754 semantics. No
// number is routed through float64 before integer representability is decided,
// and an integral value outside int64/uint64 is refused rather than rounded.
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
	value, integral, err := exactDecimalInteger(number.String())
	if err != nil {
		return nil, err
	}
	if integral {
		return value, nil
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil, malformed("JSON number is not a finite float64")
	}
	return parsed, nil
}

// exactDecimalInteger decides the exact integral value of a JSON number text.
// It accepts the JSON number grammar, including exponents and a decimal point,
// and never converts through float64. For example 9007199254740993e0 and
// 9007199254740993.0 both stay exactly 9007199254740993, while the same value
// written as a float would round to 9007199254740992. Spellings that the JSON
// grammar does not allow, such as 1., .1 or 01, are rejected instead of being
// silently normalised.
//
// integral is false when the value is finite but not an integer; the caller may
// then apply documented IEEE754 float64 semantics. err reports invalid syntax or
// an integral value outside the int64/uint64 range.
func exactDecimalInteger(text string) (value any, integral bool, err error) {
	if text == "" {
		return nil, false, malformed("empty JSON number")
	}
	negative := false
	unsigned := text
	switch unsigned[0] {
	case '-':
		negative = true
		unsigned = unsigned[1:]
	case '+':
		return nil, false, malformed("JSON number has an invalid sign")
	}
	mantissa := unsigned
	exponentText := ""
	if index := strings.IndexAny(unsigned, "eE"); index >= 0 {
		mantissa, exponentText = unsigned[:index], unsigned[index+1:]
		if mantissa == "" || exponentText == "" || strings.ContainsAny(exponentText, "eE") {
			return nil, false, malformed("JSON number is not valid")
		}
	}
	exponentNegative := false
	if exponentText != "" {
		switch exponentText[0] {
		case '-':
			exponentNegative = true
			exponentText = exponentText[1:]
		case '+':
			exponentText = exponentText[1:]
		}
		if !isDecimalDigits(exponentText) {
			return nil, false, malformed("JSON number exponent is not valid")
		}
	}
	fractionLength := 0
	if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
		if strings.IndexByte(mantissa[dot+1:], '.') >= 0 {
			return nil, false, malformed("JSON number is not valid")
		}
		integerPart := mantissa[:dot]
		fractionPart := mantissa[dot+1:]
		// JSON requires digits on both sides of the decimal point and forbids
		// leading zeroes in the integer part, so 1., .1 and 01 are malformed
		// rather than normalised input.
		if !isDecimalDigits(integerPart) || !isDecimalDigits(fractionPart) {
			return nil, false, malformed("JSON number is not valid")
		}
		if len(integerPart) > 1 && integerPart[0] == '0' {
			return nil, false, malformed("JSON number has a leading zero")
		}
		fractionLength = len(fractionPart)
		mantissa = integerPart + fractionPart
	} else {
		if !isDecimalDigits(mantissa) {
			return nil, false, malformed("JSON number is not valid")
		}
		if len(mantissa) > 1 && mantissa[0] == '0' {
			return nil, false, malformed("JSON number has a leading zero")
		}
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		// Any spelling of zero is exactly zero, whatever the exponent says.
		return int64(0), true, nil
	}
	var exponent int64
	if exponentText != "" {
		if len(exponentText) > 6 {
			// |exponent| > 999999. A non-zero value is either far outside
			// int64/uint64 or a non-integral fraction underflowing float64, so
			// this decision needs no big-integer expansion.
			if exponentNegative {
				return nil, false, nil
			}
			return nil, false, malformed("JSON integer is outside int64/uint64")
		}
		parsed, parseErr := strconv.ParseInt(exponentText, 10, 32)
		if parseErr != nil {
			return nil, false, malformed("JSON number exponent is not valid")
		}
		exponent = parsed
		if exponentNegative {
			exponent = -exponent
		}
	}
	effective := exponent - int64(fractionLength)
	if effective >= 0 {
		if effective > 20 || int64(len(digits))+effective > 20 {
			return nil, false, malformed("JSON integer is outside int64/uint64")
		}
		digits += strings.Repeat("0", int(effective))
	} else {
		scale := -effective
		if scale > int64(len(digits)) {
			return nil, false, nil
		}
		if strings.Trim(digits[len(digits)-int(scale):], "0") != "" {
			return nil, false, nil
		}
		digits = digits[:len(digits)-int(scale)]
		if digits == "" {
			return int64(0), true, nil
		}
	}
	if negative {
		parsed, parseErr := strconv.ParseInt("-"+digits, 10, 64)
		if parseErr != nil {
			return nil, false, malformed("JSON integer is outside int64")
		}
		return parsed, true, nil
	}
	parsed, parseErr := strconv.ParseUint(digits, 10, 64)
	if parseErr != nil {
		return nil, false, malformed("JSON integer is outside uint64")
	}
	if parsed <= math.MaxInt64 {
		return int64(parsed), true, nil
	}
	return parsed, true, nil
}

func isDecimalDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

func encodedSize(value any) (int64, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return int64(len(payload)), nil
}
