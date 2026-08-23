package main

import (
	"fmt"

	walkman "github.com/PAKIWASI/walkman"
)

func main() {

	walkman := walkman.NewWalkman(false, 1000, []string{".git"})

	ch := walkman.Walk("~/Documents/projects/go/walkman")

	for c := range ch {
		fmt.Println(c)
	}
}
