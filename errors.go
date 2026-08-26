// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import "fmt"

// Kind classifies a ledger error. Every integrity failure is Corrupted
// and fails closed: the store never silently returns data that did not
// verify against the in-memory root.
type Kind int

const (
	// KindBackend: the storage backend returned an error.
	KindBackend Kind = iota + 1
	// KindCorrupted: a record failed hash verification, decoding or
	// decryption. Either storage was tampered with or it is damaged.
	KindCorrupted
	// KindMissing: a record that must exist is missing (e.g. a node
	// referenced by its parent, or a root record for a requested version).
	KindMissing
	// KindInvalid: invalid input from the caller.
	KindInvalid
)

// Error is the ledger error type. Use errors.As / Error.Kind to classify.
type Error struct {
	Kind Kind
	Msg  string
	// Err carries the underlying backend error for KindBackend.
	Err error
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindBackend:
		return fmt.Sprintf("backend error: %v", e.Err)
	case KindCorrupted:
		return "corrupted record: " + e.Msg
	case KindMissing:
		return "missing record: " + e.Msg
	default:
		return "invalid input: " + e.Msg
	}
}

func (e *Error) Unwrap() error { return e.Err }

func errBackend(err error) *Error    { return &Error{Kind: KindBackend, Err: err} }
func errCorrupted(msg string) *Error { return &Error{Kind: KindCorrupted, Msg: msg} }
func errMissing(msg string) *Error   { return &Error{Kind: KindMissing, Msg: msg} }
func errInvalid(msg string) *Error   { return &Error{Kind: KindInvalid, Msg: msg} }
func errCorruptedf(f string, a ...any) *Error {
	return &Error{Kind: KindCorrupted, Msg: fmt.Sprintf(f, a...)}
}
func errInvalidf(f string, a ...any) *Error {
	return &Error{Kind: KindInvalid, Msg: fmt.Sprintf(f, a...)}
}
func errMissingf(f string, a ...any) *Error {
	return &Error{Kind: KindMissing, Msg: fmt.Sprintf(f, a...)}
}
