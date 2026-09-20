package main

import "fmt"

func main() {
	fmt.Println(describe("a1"))
	fmt.Println(exists("zz"))

	if r, err := FetchRecord("b2"); err == nil {
		fmt.Println(r.Body)
	}
}
