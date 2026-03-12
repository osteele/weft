package main

import "testing"

func TestHashChunk_StableForSameContent(t *testing.T) {
	a := hashChunk([]byte("hello\n"))
	b := hashChunk([]byte("hello\n"))
	c := hashChunk([]byte("hello\nworld\n"))

	if a != b {
		t.Fatalf("hashChunk should be stable: %d vs %d", a, b)
	}
	if a == c {
		t.Fatalf("hashChunk should differ for different content: %d vs %d", a, c)
	}
}
