package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kombifyio/homelab/internal/contractgen/runtimeinventorygen"
)

func main() {
	schema := flag.String("schema", "contracts/runtimeinventory/v1/schema.json", "canonical Homelab Runtime inventory schema")
	output := flag.String("output", "contracts/runtimeinventory/v1", "verified generation bundle directory")
	check := flag.Bool("check", false, "fail when checked-in outputs differ from generation")
	flag.Parse()

	var err error
	if *check {
		err = runtimeinventorygen.Check(*schema, *output)
	} else {
		err = runtimeinventorygen.Generate(*schema, *output)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
