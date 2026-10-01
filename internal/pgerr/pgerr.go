// Package pgerr reads PostgreSQL errors the same way whichever driver
// returned them: pgdriver's Error and pgx's PgError both carry the SQLSTATE and
// the primary message, under different method names.
package pgerr

import "errors"

// State is the SQLSTATE of a PostgreSQL error, "" for any other error.
func State(err error) string {
	var withState interface{ SQLState() string }
	if errors.As(err, &withState) {
		return withState.SQLState()
	}
	var withField interface{ Field(byte) string }
	if errors.As(err, &withField) {
		return withField.Field('C')
	}
	return ""
}

// Message is the primary message of a PostgreSQL error, without the driver's
// decoration, or err.Error() for any other error.
func Message(err error) string {
	var withField interface{ Field(byte) string }
	if errors.As(err, &withField) {
		if m := withField.Field('M'); m != "" {
			return m
		}
	}
	return err.Error()
}

// Codes this module acts on.
const (
	// LockNotAvailable is lock_timeout expiring, or NOWAIT finding a lock.
	LockNotAvailable = "55P03"
	// QueryCanceled is statement_timeout expiring, or a cancel request.
	QueryCanceled = "57014"
	// ReadOnlyTransaction is a write inside a READ ONLY transaction.
	ReadOnlyTransaction = "25006"
	// UnsafeNewEnumValue is a value added to an enum used inside the
	// transaction that added it.
	UnsafeNewEnumValue = "55P04"
	// InsufficientPrivilege is a privilege the role lacks, or, with
	// row_security off, a row-level security policy that applies to it.
	InsufficientPrivilege = "42501"
)
