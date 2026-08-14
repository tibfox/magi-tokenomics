// Command proofsvc serves merkle proofs for the distributor's share books.
package main

import (
	"flag"
	"log"
	"os"

	"magi_token/indexer/proofsvc"
)

func main() {
	addr := flag.String("addr", ":8099", "listen address")
	endpoint := flag.String("hasura", os.Getenv("HASURA_ENDPOINT"), "Hasura GraphQL endpoint")
	flag.Parse()
	if *endpoint == "" {
		log.Fatal("-hasura (or HASURA_ENDPOINT) is required")
	}
	src := proofsvc.NewHasura(*endpoint, os.Getenv("HASURA_ADMIN_SECRET"))
	log.Fatal(proofsvc.New(src).Serve(*addr))
}
