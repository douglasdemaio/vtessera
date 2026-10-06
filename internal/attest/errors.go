package attest

import "errors"

// ErrInvalid covers every way a signature can fail to establish anything.
//
// It is one error rather than a set because callers act on the outcome, not the
// mode: an unverified attestation is not verified, and every caller does the
// same thing with that. The message says which failure it was.
var ErrInvalid = errors.New("attestation is not valid")
