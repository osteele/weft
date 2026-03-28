package opscore

// Option represents an optional value that may or may not be present.
// Used for trinary logic where we need to distinguish "unknown" from "false".
type Option[T any] struct {
	value T
	valid bool
}

// Some creates an Option containing a value
func Some[T any](v T) Option[T] {
	return Option[T]{value: v, valid: true}
}

// None creates an empty Option (unknown/missing value)
func None[T any]() Option[T] {
	return Option[T]{}
}

// IsSome returns true if the Option contains a value
func (o Option[T]) IsSome() bool { return o.valid }

// IsNone returns true if the Option is empty
func (o Option[T]) IsNone() bool { return !o.valid }

// Unwrap returns the contained value (caller must check IsSome first)
func (o Option[T]) Unwrap() T { return o.value }
