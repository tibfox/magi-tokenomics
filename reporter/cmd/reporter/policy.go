package main

import (
	"encoding/json"
	"fmt"
	"os"

	"magi_token/reporter/policy"
)

// policySections are the config blocks that decide an epoch's share book, in the
// order they are hashed. Anything here is compared against the chain before the
// reporter computes anything; anything NOT here is free to differ between two
// reporters in the same quorum.
//
// The split is by whether a setting can change the numbers, not by whether it
// looks like policy:
//
//	Epoch   genesis and len decide which blocks the epoch covers at all. Already
//	        compared field-by-field by verifyChainConfig; included anyway so the
//	        digest is a complete statement of what was scored rather than a partial
//	        one that needs a footnote.
//	Source  what enters the pool: tags, exclusions, weight basis, cashout window,
//	        downvote and declined-payout handling, and the vote-mana budget.
//	Shares  how the pool is divided: curves, author split, mutes, the dust cutoff,
//	        the app tax. StakedBps is recorded rather than applied here — the split
//	        happens on-chain at claim — but it still belongs to the epoch's policy.
//	Page    pagination. It does not change the BOOK, but it changes the payloads:
//	        submitShares is attested per page, so two reporters paginating
//	        differently deadlock every page even though their books agree.
//
// Deliberately excluded: Hive/VSC/Indexer endpoints (see the policy package doc —
// the indexer one is a real residual gap for LP quorums) and everything under
// Submit, which is per-operator (its own account, its own WIF, its own progress
// file) and cannot change what the epoch pays.
func (c *Config) policySections() []any {
	return []any{c.Epoch, c.Source, c.Shares, c.Page}
}

// PolicyDigest is the value that must match policy|<channel>|<epoch> on chain.
func (c *Config) PolicyDigest() (string, error) {
	return policy.Digest(c.policySections()...)
}

// cmdPolicyDigest prints the digest of this config.
//
// Two uses: the value to pass to addChannel/setPolicy when configuring a channel,
// and the value to compare across the machines in an Attest quorum. If two
// reporters print different digests they WILL score epochs differently, and the
// difference is somewhere in the sections listed by policySections.
func cmdPolicyDigest(cfg *Config, asJSON bool) error {
	d, err := cfg.PolicyDigest()
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"policy":  d,
			"channel": cfg.Contracts.Channel,
		})
	}
	fmt.Printf("policy digest: %s\n", d)
	fmt.Printf("channel:       %s\n", cfg.Contracts.Channel)
	fmt.Println()
	fmt.Println("Pass this to addChannel (\"policy\") when creating the channel, or to")
	fmt.Println("setPolicy to change it. Every reporter on this channel must print the")
	fmt.Println("same value — a differing one is refused rather than allowed to disagree.")
	return nil
}
