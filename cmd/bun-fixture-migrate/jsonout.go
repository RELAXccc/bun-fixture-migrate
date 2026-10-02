package main

import (
	"encoding/json"
	"io"
)

// writeJSON writes a report as -json prints it: what the library returned,
// encoded, indented.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
