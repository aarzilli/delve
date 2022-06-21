package main

import (
	"fmt"
	"os"
	"time"
)

func f() {
	for {
		time.Sleep(1 * time.Second)
		if len(os.Args) > 1 {
			break
		}
		fmt.Printf("ping!\n")
	}
}

func main() {
	f()
}
