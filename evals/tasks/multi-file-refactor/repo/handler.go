package main

import "fmt"

func describe(id string) string {
	r, err := FetchRecord(id)
	if err != nil {
		return "unknown: " + err.Error()
	}
	return fmt.Sprintf("%s => %s", r.ID, r.Body)
}

func exists(id string) bool {
	_, err := FetchRecord(id)
	return err == nil
}
