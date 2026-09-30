package fixturemigrate

import (
	"math/big"
	"strconv"
	"strings"
	"time"
)

// A value's text is what gets compared and what a migration writes, so it has
// to be exactly the value, spelled one way.
//
// For a fixture scalar the YAML type decides, because that is what decides
// what dbfixture hands bun: a string is kept byte for byte, so "01234" stays a
// postcode and "  x  " keeps its spaces; an integer is resolved the way YAML
// resolves it, so 017 is 15 and 0x1F is 31, as dbfixture stores them; a
// decimal is written out exactly, never through float64, so 9007199254740993
// stays itself. On the database side the column type decides: numbers are
// written the same way, everything else is PostgreSQL's own text.

// scalarText is the text of a fixture scalar by its YAML type.
func scalarText(c Cell) string {
	switch c.Tag {
	case "!!int":
		if s, ok := yamlInt(c.Text); ok {
			return s
		}
	case "!!float":
		if s, ok := yamlFloat(c.Text); ok {
			return s
		}
	case "!!bool":
		return strings.ToLower(c.Text)
	case "!!timestamp":
		if s, ok := yamlTimestamp(c.Text); ok {
			return s
		}
	}
	return c.Text
}

// yamlTimestampLayouts are yaml.v3's (resolve.go, allowedTimestampFormats).
var yamlTimestampLayouts = []string{
	"2006-1-2T15:4:5.999999999Z07:00",
	"2006-1-2t15:4:5.999999999Z07:00",
	"2006-1-2 15:4:5.999999999",
	"2006-1-2",
}

// yamlTimestamp resolves a YAML timestamp the way yaml.v3 does and writes it
// one way: a date alone as 2006-01-02, anything with a time as RFC 3339 in
// UTC. A timestamp without a zone is UTC to yaml.v3, and so to dbfixture, and
// bun writes the time.Time it becomes with its offset: that instant is what
// the database holds.
func yamlTimestamp(text string) (string, bool) {
	for _, layout := range yamlTimestampLayouts {
		t, err := time.Parse(layout, text)
		if err != nil {
			continue
		}
		if layout == "2006-1-2" {
			return t.Format("2006-01-02"), true
		}
		return t.UTC().Format(time.RFC3339Nano), true
	}
	return "", false
}

// exportTimestamp writes a date or a timestamp from the database as a YAML
// timestamp, which yaml.v3 decodes into a time.Time field; a quoted string it
// only decodes when it is RFC 3339. The second result is false for a value
// that is not one, such as infinity, which stays a string.
func exportTimestamp(typ, text string) (string, bool) {
	switch typ {
	case "date":
		if t, err := time.Parse("2006-01-02", text); err == nil {
			return t.Format("2006-01-02"), true
		}
	case "timestamp":
		if t, err := time.Parse("2006-01-02 15:04:05.999999999", text); err == nil {
			return t.Format("2006-01-02 15:04:05.999999999"), true
		}
	case "timestamptz":
		for _, layout := range []string{"2006-01-02 15:04:05.999999999Z07", "2006-01-02 15:04:05.999999999Z07:00"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UTC().Format(time.RFC3339Nano), true
			}
		}
	}
	return "", false
}

// yamlInt resolves a YAML integer exactly: the sign, the 0x, 0o and 0b
// prefixes, a leading 0 as octal and the underscores yaml.v3 accepts, with no
// limit on the size. yaml.v3 parses with strconv.ParseInt(plain, 0, 64), which
// has the same rules as big.Int.SetString(plain, 0).
func yamlInt(text string) (string, bool) {
	plain := strings.ReplaceAll(text, "_", "")
	n, ok := new(big.Int).SetString(plain, 0)
	if !ok {
		return "", false
	}
	return n.String(), true
}

// yamlFloat resolves a YAML float exactly, and spells infinity and NaN the way
// PostgreSQL does, which is what bun writes for a float64 holding one.
func yamlFloat(text string) (string, bool) {
	switch text {
	case ".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF":
		return "Infinity", true
	case "-.inf", "-.Inf", "-.INF":
		return "-Infinity", true
	case ".nan", ".NaN", ".NAN":
		return "NaN", true
	}
	return canonicalDecimal(strings.ReplaceAll(text, "_", ""))
}

// maxExponent bounds the exponent canonicalDecimal expands, so "1e999999999"
// cannot make it write a billion zeros.
const maxExponent = 1000

// canonicalDecimal writes a decimal number in one spelling: no sign for zero
// or a positive number, no leading zeros, no trailing zeros after the point,
// no point for an integer, no exponent. It is exact: "1.0", "1e0" and "10e-1"
// are all "1", and 9007199254740993 stays 9007199254740993. The second result
// is false for anything that is not a decimal number.
func canonicalDecimal(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg, s = true, s[1:]
	}
	mantissa, exponent := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > maxExponent || e < -maxExponent {
			return "", false
		}
		exponent = e
	}
	intPart, fracPart := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart, fracPart = mantissa[:i], mantissa[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return "", false
	}
	for _, part := range []string{intPart, fracPart} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", false
			}
		}
	}
	// digits × 10^scale, with the point moved into the exponent.
	digits := strings.TrimLeft(intPart+fracPart, "0")
	scale := exponent - len(fracPart)
	if digits == "" {
		return "0", true
	}
	for len(digits) > 1 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		scale++
	}
	var out string
	switch {
	case scale >= 0:
		out = digits + strings.Repeat("0", scale)
	case -scale < len(digits):
		out = digits[:len(digits)+scale] + "." + digits[len(digits)+scale:]
	default:
		out = "0." + strings.Repeat("0", -scale-len(digits)) + digits
	}
	if neg {
		out = "-" + out
	}
	return out, true
}

// numericType reports a PostgreSQL type whose values compare as numbers.
func numericType(typ string) bool {
	switch typ {
	case "int2", "int4", "int8", "numeric", "float4", "float8", "money", "oid":
		return true
	}
	return false
}

// columnText is how a value of a column of this type is compared: a number
// canonically, anything else as it is.
func columnText(typ, text string) string {
	if numericType(typ) {
		if s, ok := canonicalDecimal(text); ok {
			return s
		}
	}
	return text
}

// sameScalar reports whether two texts are the same value when either could
// be a number: the zero checks compare a value with "0", "false" or "".
func sameScalar(a, b string) bool {
	if a == b {
		return true
	}
	ca, okA := canonicalDecimal(a)
	cb, okB := canonicalDecimal(b)
	return okA && okB && ca == cb
}
