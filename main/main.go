package main

import (
	"fmt"

	walkman "github.com/PAKIWASI/walkman"
)

func main() {
	wm := walkman.NewWalkman(false, 1000, []string{".git"})

	ch := wm.Walk("/")

	for c := range ch {
		if c.Err != nil {
			fmt.Println("error:", c.Dir, c.Err)
			continue
		}
		for _, entry := range c.Ret {
			kind := "f"
			if entry.IsDir() {
				kind = "d"
			}
			fmt.Printf("%s  %s\n", kind, c.Dir+"/"+entry.Name())
		}
	}

	if err := wm.Wait(); err != nil {
		fmt.Println("walk failed:", err)
	}
}
