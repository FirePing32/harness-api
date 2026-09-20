package main

import "testing"

func TestLookup(t *testing.T) {
	if _, err := FetchRecord("a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchRecord("nope"); err == nil {
		t.Fatal("expected an error for a missing id")
	}
}
