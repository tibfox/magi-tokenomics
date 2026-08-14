package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// One distributor now serves several reward channels instead of one contract per
// channel. These cover the seams that creates: two channels sharing a contract must
// not share funding, share books, claims, or reporter authority.

const mdDist = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"

// mdSetup wires token + C2 + one distributor funded by TWO buckets — `content` and
// `lp` — both paying the same contract. That configuration is the whole point of the
// merge and was impossible before C2 keyed allocations by bucket name.
func mdSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(mdDist, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"content","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:6000,lp:contract:%s:4000"}`,
		tokenID, mdDist, mdDist), owner, 0, true)
	call(t, &ct, mdDist, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
		tokenID, c2ID), owner, 0, true)
	call(t, &ct, mdDist, "addChannel", `{"channel":"content","bucket":"content","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:creporter","reporterThreshold":"1","role":"content"}`, owner, 0, true)
	call(t, &ct, mdDist, "addChannel", `{"channel":"lp","bucket":"lp","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:lpreporter","reporterThreshold":"1","role":"lp"}`, owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	return &ct
}

// ONE emission, TWO channels in one contract, each paid from its own bucket and its
// own share book. Previously this needed two deployed contracts.
func TestMergedDist_TwoChannelsOneContract(t *testing.T) {
	ct := mdSetup(t)
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)

	// each channel pulls ITS bucket: 100000 emission split 6000/4000 bps
	fc := call(t, ct, mdDist, "pullFunding", `{"channel":"content","epoch":"0"}`, "hive:anyone", 1, true)
	fl := call(t, ct, mdDist, "pullFunding", `{"channel":"lp","epoch":"0"}`, "hive:anyone", 1, true)
	assert.EqualValues(t, 60000, c17I64(t, fc.Ret, "funded"), "content bucket: "+fc.Ret)
	assert.EqualValues(t, 40000, c17I64(t, fl.Ret, "funded"), "lp bucket: "+fl.Ret)

	// separate reporters, separate share books
	cb := publishEntries(t, ct, mdDist, "content", "0", "hive:alice:100", "hive:creporter", 1)
	lb := publishEntries(t, ct, mdDist, "lp", "0", "hive:bob:100", "hive:lpreporter", 1)
	call(t, ct, mdDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:creporter", 1, true)
	call(t, ct, mdDist, "finalizeEpoch", `{"channel":"lp","epoch":"0"}`, "hive:lpreporter", 1, true)

	// alice earned in content only, bob in lp only — each gets that channel's pot
	assert.EqualValues(t, 60000, c17I64(t,
		call(t, ct, mdDist, "claim", cb.claimFor(t, "content", "0", "hive:alice"), "hive:alice", 5, true).Ret, "claimed"))
	assert.EqualValues(t, 40000, c17I64(t,
		call(t, ct, mdDist, "claim", lb.claimFor(t, "lp", "0", "hive:bob"), "hive:bob", 5, true).Ret, "claimed"))

	// and neither can claim in the other's channel
	// A proof is bound to ONE channel's root: alice's content proof is worthless
	// against the lp book even though both roots exist on the same contract.
	call(t, ct, mdDist, "claim", cb.claimFor(t, "lp", "0", "hive:alice"), "hive:alice", 5, false)
	call(t, ct, mdDist, "claim", lb.claimFor(t, "content", "0", "hive:bob"), "hive:bob", 5, false)
}

// Reporter authority is per channel. The LP reporter must not be able to write the
// content share book, which is the same key space one contract away.
func TestMergedDist_ReporterAuthorityIsPerChannel(t *testing.T) {
	ct := mdSetup(t)
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, ct, mdDist, "pullFunding", `{"channel":"content","epoch":"0"}`, "hive:anyone", 1, true)

	// the LP reporter submitting to content is refused...
	call(t, ct, mdDist, "submitShares",
		`{"channel":"content","epoch":"0","page":"0","entries":"hive:mallory:999"}`, "hive:lpreporter", 1, false)
	// ...and cannot finalize it either
	call(t, ct, mdDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:lpreporter", 1, false)

	// Nothing was committed: no root, so no claim against this epoch could ever
	// verify. Under the merkle book that is the observable, not a per-account share.
	sh := call(t, ct, mdDist, "shareOf",
		`{"channel":"content","epoch":"0","account":"hive:mallory"}`, "hive:probe", 1, true)
	assert.Contains(t, sh.Ret, `"root":""`, "no commitment should exist: "+sh.Ret)
	assert.Contains(t, sh.Ret, `"totalShares":"0"`, "nothing should have been written: "+sh.Ret)
}

// A channel must exist before it can be used. A typo'd name would otherwise open a
// fresh, unfunded namespace whose epochs read as empty rather than wrong.
func TestMergedDist_UnknownChannelRefused(t *testing.T) {
	ct := mdSetup(t)
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, ct, mdDist, "pullFunding", `{"channel":"contnet","epoch":"0"}`, "hive:anyone", 1, false)
	call(t, ct, mdDist, "submitShares",
		`{"channel":"","epoch":"0","page":"0","entries":"hive:a:1"}`, "hive:creporter", 1, false)
}

// Channels are append-only and their buckets are exclusive: re-pointing a live
// channel, or aiming two channels at one bucket, would rewrite the rules under an
// epoch already in flight.
func TestMergedDist_ChannelsAreAppendOnlyAndBucketExclusive(t *testing.T) {
	ct := mdSetup(t)
	// re-adding an existing channel is refused
	call(t, ct, mdDist, "addChannel", `{"channel":"content","bucket":"content","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:attacker","reporterThreshold":"1"}`, owner, 1, false)
	// a second channel on an already-used bucket is refused
	call(t, ct, mdDist, "addChannel", `{"channel":"content2","bucket":"content","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:x","reporterThreshold":"1"}`, owner, 1, false)
	// a bucket the funder does not know is refused
	call(t, ct, mdDist, "addChannel", `{"channel":"ghost","bucket":"nosuch","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:x","reporterThreshold":"1"}`, owner, 1, false)
	// and only the owner may add one at all
	call(t, ct, mdDist, "addChannel", `{"channel":"evil","bucket":"lp","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:x","reporterThreshold":"1"}`, "hive:mallory", 1, false)

	// the original channel is untouched
	ci := call(t, ct, mdDist, "channelInfo", `{"channel":"content"}`, "hive:probe", 1, true)
	assert.Contains(t, ci.Ret, `"bucket":"content"`)
	assert.Contains(t, ci.Ret, `"role":"content"`)
}
