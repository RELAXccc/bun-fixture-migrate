package fixtureapply

import (
	"bytes"
	"encoding/json"
	"errors"
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
	if _, err := writeArray(&b, raw, 1); err != nil {
		return "", true, err
	}
	return b.String(), true, nil
}

// maxDimensions is the most dimensions a PostgreSQL array has (MAXDIM).
const maxDimensions = 6

// writeArray writes one level of an array, the depth'th, and returns its
// shape: its length, then the shape of its elements when they are arrays.
// PostgreSQL only stores arrays whose sub-arrays all have one shape.
func writeArray(b *strings.Builder, raw json.RawMessage, depth int) ([]int, error) {
	if depth > maxDimensions {
		return nil, errTooDeep
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	if len(elems) == 0 && depth > 1 {
		return nil, errEmptyRow
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
			shape, err := writeArray(b, e, depth+1)
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
			if err := quoteElement(b, s); err != nil {
				return nil, err
			}
		case '{':
			var compact bytes.Buffer
			if err := json.Compact(&compact, e); err != nil {
				return nil, err
			}
			if err := quoteElement(b, compact.String()); err != nil {
				return nil, err
			}
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

var (
	errRagged = errors.New("its elements are not all arrays of one length, and PostgreSQL only stores an array " +
		"whose rows all have the same length")
	errTooDeep = fmt.Errorf("it nests lists more than %d deep, while PostgreSQL stores arrays of at most %d "+
		"dimensions", maxDimensions, maxDimensions)
	errEmptyRow = errors.New("it holds an empty list inside a list, which PostgreSQL cannot store: an empty " +
		"array has no dimensions, so it cannot be a row of another")
	// errNUL is the sentence Validate refuses any value holding a NUL with.
	errNUL = errors.New("the value holds a NUL character, which PostgreSQL cannot store")
)

// quoteElement writes s as a quoted element of an array literal. A NUL is
// refused: PostgreSQL's text cannot hold one, and bun v1.2.18 drops it from a
// bound string without a word, so ["a\u0000b"] would be stored as {ab}.
func quoteElement(b *strings.Builder, s string) error {
	if strings.ContainsRune(s, 0) {
		return errNUL
	}
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return nil
}
