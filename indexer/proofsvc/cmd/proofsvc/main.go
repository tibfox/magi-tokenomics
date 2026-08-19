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
	// Scope every query to ONE distributor. Required whenever an indexer serves more
	// than one deployment: channel names are conventional and every deployment numbers
	// epochs from 0, so two tenants collide on their first epoch and each poisons the
	// other's book. Empty = unscoped, which is correct for a single-tenant indexer.
	contract := flag.String("distributor", os.Getenv("MAGI_DISTRIBUTOR"),
		"distributor contract id to scope queries to (empty = unscoped, single-tenant indexer)")
	flag.Parse()
	if *endpoint == "" {
		log.Fatal("-hasura (or HASURA_ENDPOINT) is required")
	}
	src := proofsvc.NewHasura(*endpoint, os.Getenv("HASURA_ADMIN_SECRET"))
	src.Contract = *contract
	log.Fatal(proofsvc.New(src).Serve(*addr))
}
