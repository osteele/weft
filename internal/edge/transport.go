package edge

import (
	"context"
	"errors"
)

// ErrNotFound marks a confirmed absence: the store was reachable and the object
// is not there. Any other error means the store could not be consulted, and the
// object's existence is unknown.
//
// Keeping these apart matters because callers act on absence — a missing
// payload refuses a submission, a missing ack keeps a wait running — and an
// unreachable store must never be read as "confirmed gone".
var ErrNotFound = errors.New("edge: object not found")

// ErrAlreadyExists is returned by PutIfAbsent when the key is taken. For a
// pointer write it is not a failure: it means this submission was already
// committed, which is the idempotency guarantee working.
var ErrAlreadyExists = errors.New("edge: object already exists")

// Transport is the object store the protocol runs over: get, list, put, and a
// conditional put that is the commit point for a submission.
//
// Two implementations exist. R2 is production. The filesystem implementation
// backs tests and offline development, and keeps signature verification fully
// enabled with test keys rather than bypassing it, so the verify-then-parse
// ordering is exercised on every test run.
type Transport interface {
	// Name identifies the transport in operator-facing output.
	Name() string
	// Get returns an object, or ErrNotFound if the store was reachable and the
	// object is absent.
	Get(ctx context.Context, key string) ([]byte, error)
	// List returns keys under a prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Put writes an object unconditionally. Used for content-addressed
	// payloads, where a repeated write is harmless.
	Put(ctx context.Context, key string, body []byte) error
	// PutIfAbsent writes an object only if the key is free, returning
	// ErrAlreadyExists otherwise. This is the atomic commit point for a
	// submission pointer.
	PutIfAbsent(ctx context.Context, key string, body []byte) error
	// Delete removes an object.
	Delete(ctx context.Context, key string) error
}

// Key layout. The nonce is the pointer key, so committing a submission and
// claiming its idempotency identity are the same operation.
const (
	prefixRoot    = "edge/v1/"
	PrefixInbox   = prefixRoot + "inbox/"
	PrefixPayload = prefixRoot + "payload/"
	PrefixAck     = prefixRoot + "ack/"
)

// There is deliberately no key prefix in this store.
//
// The hub's keyring is local and authoritative. If verification keys were read
// from the inbound bucket, write access to that bucket would become
// key-registration access, and an attacker who can write it would register a
// key and sign whatever it liked — every other check would still run, while
// authentication silently became a formality. That is worse than having no
// signatures, because the refusal messages would then assert an identity
// nobody verified.
//
// The general form, which also covers acknowledgements: a fact stored where an
// untrusted party can write it must never be read back as authority.

func InboxKey(nonce string) string    { return PrefixInbox + nonce }
func PayloadKey(digest string) string { return PrefixPayload + digest }
func AckKey(nonce string) string      { return PrefixAck + nonce + ".json" }
