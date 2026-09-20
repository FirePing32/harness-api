package oai

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Unknown-field preservation.
//
// We sit between a client and an arbitrary OpenAI-compatible provider, and both
// ends invent fields we have not modelled. Dropping them silently corrupts a
// conversation in ways that only show up several turns later, so every type on the
// hot path captures what it does not recognise into an Extra map and re-emits it.
//
// The set of "known" keys is derived by reflection over the struct tags rather
// than hand-maintained, because a hand-maintained list drifts the moment someone
// adds a field and forgets to register it — and the failure is silent duplication
// of that key in the output.

// knownFields returns the JSON key names declared by v's struct tags.
func knownFields(v any) map[string]struct{} {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	out := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			out[name] = struct{}{}
		}
	}
	return out
}

// captureExtra returns the members of a JSON object that are not in known.
// A nil result means there was nothing unrecognised, which keeps the common case
// allocation-free on the way back out.
func captureExtra(data []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for k, v := range all {
		if _, ok := known[k]; ok {
			continue
		}
		if extra == nil {
			extra = make(map[string]json.RawMessage, 2)
		}
		extra[k] = v
	}
	return extra, nil
}

// mergeExtra splices extra keys back into an already-marshalled object. Keys that
// the struct itself emitted win, so a field we model can never be shadowed by a
// stale captured copy of itself.
func mergeExtra(marshalled []byte, extra map[string]json.RawMessage, omit ...string) ([]byte, error) {
	if len(extra) == 0 && len(omit) == 0 {
		return marshalled, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(marshalled, &obj); err != nil {
		return nil, fmt.Errorf("merge extra fields: %w", err)
	}
	for k, v := range extra {
		if _, exists := obj[k]; !exists {
			obj[k] = v
		}
	}
	for _, k := range omit {
		delete(obj, k)
	}
	return json.Marshal(obj)
}
