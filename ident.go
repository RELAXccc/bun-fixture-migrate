package fixturemigrate

import (
	"fmt"
	"regexp"
	"strings"
)

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// quoteIdent double-quotes a plain SQL identifier and rejects anything else.
// Every identifier this tool puts into a query comes from the configuration
// file or from the catalog, but this is the line between them and the database
// and it is cheap to hold.
func quoteIdent(name string) (string, error) {
	if !identPattern.MatchString(name) {
		return "", fmt.Errorf("%q is not a plain SQL identifier", name)
	}
	return `"` + name + `"`, nil
}

// quoteQualified does the same for an optionally schema-qualified name.
func quoteQualified(name string) (string, error) {
	parts := strings.Split(name, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		q, err := quoteIdent(p)
		if err != nil {
			return "", fmt.Errorf("%q is not a plain SQL identifier", name)
		}
		out = append(out, q)
	}
	return strings.Join(out, "."), nil
}
