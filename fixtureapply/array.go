package fixtureapply

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// arrayLiteral turns a JSON array, which is how a YAML sequence arrives, into
// the text of a PostgreSQL array of the same shape: [[1,2],[3,4]] is
// {{1,2},{3,4}}, which a cast to integer[] reads as two dimensions.
// PostgreSQL's jsonb_array_elements_text unpacks one level only, and hands
// the inner arrays on as text no element type can read.
//
// A string is always quoted, so "NULL" stays the string and "" stays empty;
// null is SQL NULL; a number or a boolean is written as the JSON spells it;
// an object, for a json[] or jsonb[] column, is its JSON text. ok is false for
// text that is not a JSON array, which is left for PostgreSQL to read as it
// stands, as it does '[0:1]={7,8}'.
func arrayLiteral(text string) (literal string, ok bool, err error) {
	raw := json.RawMessage(strings.TrimSpace(text))
	if !json.Valid(raw) || raw[0] != '[' {
		return "", false, nil
	}
	var b strings.Builder
	if _, err := writeArray(&b, raw); err != nil {
		return "", true, err
	}
	return b.String(), true, nil
}

// writeArray writes one level of an array and returns its shape: its length,
// then the shape of its elements when they are arrays. PostgreSQL only stores
// arrays whose sub-arrays all have one shape.
func writeArray(b *strings.Builder, raw json.RawMessage) ([]int, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	b.WriteByte('{')
	var inner []int
	nested := 0
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		e = bytes.TrimSpace(e)
		switch e[0] {
		case '[':
			shape, err := writeArray(b, e)
			if err != nil {
				return nil, err
			}
			if nested > 0 && fmt.Sprint(shape) != fmt.Sprint(inner) {
				return nil, errRagged
			}
			inner = shape
			nested++
		case '"':
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return nil, err
			}
			quoteElement(b, s)
		case '{':
			var compact bytes.Buffer
			if err := json.Compact(&compact, e); err != nil {
				return nil, err
			}
			quoteElement(b, compact.String())
		default:
			if string(e) == "null" {
				b.WriteString("NULL")
				continue
			}
			b.Write(e)
		}
	}
	if nested > 0 && nested != len(elems) {
		return nil, errRagged
	}
	b.WriteByte('}')
	return append([]int{len(elems)}, inner...), nil
}

var errRagged = fmt.Errorf("its elements are not all arrays of one length, and PostgreSQL only stores an array " +
	"whose rows all have the same length")

// quoteElement writes s as a quoted element of an array literal.
func quoteElement(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
}
