package main

import "fmt"

type Record struct {
	ID   string
	Body string
}

var records = map[string]Record{
	"a1": {ID: "a1", Body: "first"},
	"b2": {ID: "b2", Body: "second"},
}

// FetchRecord returns the record with the given id.
func FetchRecord(id string) (Record, error) {
	r, ok := records[id]
	if !ok {
		return Record{}, fmt.Errorf("no record %q", id)
	}
	return r, nil
}
