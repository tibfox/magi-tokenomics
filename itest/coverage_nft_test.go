package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Coverage: the editioned-NFT value asset (adapter kind "1") FAILS CLOSED.
//
// Decision (B): NFT mode is not implemented. The magi_nft-contract interface can
// mechanically satisfy Transfer/PullFrom/Mint, but the framework prerequisites the
// plan itself mandates for that mode do NOT exist — R4's integer fractional-carry
// accumulator (per-bucket in C2, per-claimant in C3/C5/C7), R4's "reject configs
// where per-epoch emission < #buckets" init check, R16's soulbound rejection, and
// kind-aware supply reads in C2 (the NFT has no global maxSupply/totalSupply, both
// are per-id). Shipping an adapter-only NFT path would let a deployment init
// cleanly, emit, and then permanently strand every claimant whose pro-rata slice
// truncates below one whole edition. So instead every contract now rejects kind
// "1" at init.
//
// These tests prove the gate end-to-end on the real VSC engine:
//   - every one of the 3 contracts rejects kind "1" at init, with the explicit
//     NFT-unsupported message (not a generic parse failure);
//   - unknown/absent kinds are rejected too (no silent fallthrough to fungible);
//   - a non-empty tokenId is rejected (it only ever meant "the NFT edition id");
//   - the very same payload with kind "0" still inits — so the rejection is
//     attributable to `kind` alone and fungible mode is untouched.

const (
	nftTokenID = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSNFTtk"
	nftC1ID    = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSNFTk1"
	nftC2ID    = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSNFTk2"
	nftC3ID    = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSNFTk3"
	nftOwner   = "hive:tibfox"
)

// nftInitPayloads returns, per contract id, an init payload template containing a
// literal %KIND% / %TOKENID% placeholder so the SAME payload can be replayed with
// a rejected and an accepted kind — that is what makes the rejection attributable.
func nftInitPayloads() []struct {
	name string
	id   string
	tmpl string
} {
	return []struct {
		name string
		id   string
		tmpl string
	}{
		{"c1-staking", nftC1ID,
			fmt.Sprintf(`{"token":"%s","kind":"%%KIND%%","tokenId":"%%TOKENID%%","cooldown":"2","epochLen":"1","allow":"","maxAirdrop":"1000000"}`, nftTokenID)},
		{"c2-emission", nftC2ID,
			fmt.Sprintf(`{"token":"%s","kind":"%%KIND%%","tokenId":"%%TOKENID%%","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:6000,yield:contract:%s:4000"}`,
				nftTokenID, nftC3ID, nftC1ID)},
		{"c3-distributor", nftC3ID,
			fmt.Sprintf(`{"token":"%s","kind":"%%KIND%%","tokenId":"%%TOKENID%%","funder":"%s","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
				nftTokenID, nftC2ID)},
	}
}

func nftPayload(tmpl, kind, tokenId string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tmpl, "%KIND%", kind), "%TOKENID%", tokenId)
}

// nftNewChain boots a fresh engine with the token + all 3 framework contracts
// registered and the token initialized.
func nftNewChain(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(nftTokenID, nftOwner, read(tokenWasmPath))
	ct.RegisterContract(nftC1ID, nftOwner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(nftC2ID, nftOwner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(nftC3ID, nftOwner, read("../c3-distributor/artifacts/main.wasm"))
	call(t, &ct, nftTokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, nftOwner, 0, true)
	return &ct
}

// TestCovNFT_EveryContractRejectsNFTKindAtInit — the core fail-closed proof.
// Each contract is offered kind "1" and must abort with the explicit
// NFT-unsupported message; the identical payload with kind "0" must then init.
func TestCovNFT_EveryContractRejectsNFTKindAtInit(t *testing.T) {
	ct := nftNewChain(t)

	// Phase 1: every contract rejects kind "1". Aborts revert, so `init` is not
	// consumed and phase 2 can still init the same contract.
	for _, c := range nftInitPayloads() {
		res := call(t, ct, c.id, "init", nftPayload(c.tmpl, "1", ""), nftOwner, 0, false)
		msg := res.Ret + " " + res.ErrMsg
		assert.Contains(t, msg, "editioned-NFT value asset (kind=1) is not supported",
			c.name+": kind=1 must be rejected with the explicit NFT message, got: "+msg)
	}

	// Phase 2: the SAME payloads with kind "0" init successfully. Ordering matters:
	// C2 reads the token's getInfo, and C3/C5/C7 read C2's scheduleInfo at init.
	order := map[string]int{"c1-staking": 0, "c2-emission": 1, "c3-distributor": 2}
	seq := make([]struct {
		name string
		id   string
		tmpl string
	}, 3)
	for _, c := range nftInitPayloads() {
		seq[order[c.name]] = c
	}
	for _, c := range seq {
		call(t, ct, c.id, "init", nftPayload(c.tmpl, "0", ""), nftOwner, 0, true)
		// C1 adopts the emission schedule the moment C2 exists. C7's init below refuses
		// a stakeSource that is not accumulating drawdowns, because without them its
		// yield denominator over-counts and part of every epoch is stranded.
		if c.name == "c2-emission" {
			call(t, ct, nftC1ID, "adoptSchedule",
				fmt.Sprintf(`{"funder":"%s"}`, nftC2ID), nftOwner, 0, true)
		}
	}
}

// TestCovNFT_UnknownKindRejected — a kind that is neither "0" nor "1" (and an
// absent one) must abort rather than silently fall through to fungible. Without
// this, `kind:"nft"` or a dropped field would deploy as a fungible contract
// pointed at an NFT id.
func TestCovNFT_UnknownKindRejected(t *testing.T) {
	ct := nftNewChain(t)
	for _, c := range nftInitPayloads() {
		for _, bad := range []string{"2", "", "nft", "01", " 0"} {
			res := call(t, ct, c.id, "init", nftPayload(c.tmpl, bad, ""), nftOwner, 0, false)
			msg := res.Ret + " " + res.ErrMsg
			assert.Contains(t, msg, `kind must be "0" (fungible)`,
				fmt.Sprintf("%s: kind=%q must be rejected as an unknown kind, got: %s", c.name, bad, msg))
		}
	}
}

// TestCovNFT_TokenIdRejectedInFungibleMode — `tokenId` only ever meant "the single
// editioned NFT id". Accepting it while running fungible would let an operator
// believe they had configured NFT mode. It must be rejected even with kind "0".
func TestCovNFT_TokenIdRejectedInFungibleMode(t *testing.T) {
	ct := nftNewChain(t)
	for _, c := range nftInitPayloads() {
		res := call(t, ct, c.id, "init", nftPayload(c.tmpl, "0", "edition-1"), nftOwner, 0, false)
		msg := res.Ret + " " + res.ErrMsg
		assert.Contains(t, msg, "tokenId is only meaningful",
			c.name+": a non-empty tokenId must be rejected, got: "+msg)
	}
}

// TestCovNFT_FungibleEmissionAndClaimStillWorks — regression guard: the fail-closed
// gate must not disturb the fungible path. Full C2→C3 emission + claim on the
// fresh id set, ending in real token balances.
func TestCovNFT_FungibleEmissionAndClaimStillWorks(t *testing.T) {
	ct := nftNewChain(t)
	all := nftInitPayloads()
	byName := map[string]string{}
	for _, c := range all {
		byName[c.name] = c.tmpl
	}
	fundC2Pool(t, ct, nftTokenID, nftC2ID, "500000000", 0)
	call(t, ct, nftC2ID, "init", nftPayload(byName["c2-emission"], "0", ""), nftOwner, 0, true)
	call(t, ct, nftC3ID, "init", nftPayload(byName["c3-distributor"], "0", ""), nftOwner, 0, true)
	call(t, ct, nftC3ID, "addChannel", `{"channel":"author","bucket":"author","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1"}`, nftOwner, 0, true)
	call(t, ct, nftC1ID, "init", nftPayload(byName["c1-staking"], "0", ""), nftOwner, 0, true)
	call(t, ct, nftC1ID, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, nftC2ID), nftOwner, 0, true)

	// C2 becomes the token owner so it can mint.
	call(t, ct, nftTokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, nftC2ID), nftOwner, 0, true)

	// epoch 0 at height 1 → emission = baseAnnual*epochLen/blocksPerYear = 100000,
	// split 6000/4000 bps → author bucket (C3) = 60000.
	call(t, ct, nftC2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, ct, nftC3ID, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:anyone", 1, true)
	bk1 := publishEntries(t, ct, nftC3ID, "author", "0", "hive:alice:75,hive:bob:25", "hive:reporter", 1)
	call(t, ct, nftC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 1, true)
	call(t, ct, nftC3ID, "claim", bk1.claimFor(t, "author", "0", "hive:alice"), "hive:alice", 2, true)
	call(t, ct, nftC3ID, "claim", bk1.claimFor(t, "author", "0", "hive:bob"), "hive:bob", 2, true)

	a := call(t, ct, nftTokenID, "balanceOf", `{"account":"hive:alice"}`, "hive:x", 2, true)
	b := call(t, ct, nftTokenID, "balanceOf", `{"account":"hive:bob"}`, "hive:x", 2, true)
	assert.Contains(t, a.Ret, `"45000"`, "alice should get 75% of the 60000 author bucket")
	assert.Contains(t, b.Ret, `"15000"`, "bob should get 25% of the 60000 author bucket")
}
