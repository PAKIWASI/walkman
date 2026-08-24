package main

import (
	"fmt"
	"path/filepath"

	walkman "github.com/PAKIWASI/walkman"
)

func main() {

	walkman := walkman.NewWalkman(false, 1000, []string{})

	ch := walkman.Walk("/home/wasi")

	for c := range ch {
		if c.Err != nil {
			fmt.Println("error:", c.Err)
			continue
		}
		for _, entry := range c.Ret {
			kind := "f"
			if entry.IsDir() {
				kind = "d"
			}
			fmt.Printf("%s  %s\n", kind, filepath.Join(c.Dir, entry.Name()))
		}
	}
}
