package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// releaseStaleAttest must not be usable against an action it does not name.
//
// The authority set comes from a caller-supplied CHANNEL (reporterCfg(ch)) while the
// action key is a separate free-form string. Name channel B while releasing channel
// A's action and clearRound deletes `rep|aseen|<action>|<auth>` for B's authorities —
// so A's real voters keep their markers.
//
// That is terminal, and permissionless. The round's astart and ahashes ARE deleted,
// so a corrective release finds "no attestation round in progress"; the real voters
// still hold aseen, so each gets "caller already attested this action". The action
// can never be voted on again and never released again, and for an `sr:` action that
// means the epoch never gets a root and therefore can never be finalized.
const (
	rlTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	rlC2  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	rlC3  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
	rlPol = "3333333333333333333333333333333333333333333333333333333333333333"
)

func rlState(t *testing.T, ct *test_utils.ContractTest, k string) string {
	t.Helper()
	return strings.Trim(ct.StateGet(rlC3, k), `"`)
}

func TestReleaseScope_WrongChannelCannotClearAnotherChannelsRound(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(rlTok, owner, read(tokenWasmPath))
	ct.RegisterContract(rlC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(rlC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, rlTok, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, rlTok, rlC2, "500000000", 0)
	call(t, &ct, rlC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"author","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"author:contract:%s:5000,lp:contract:%s:5000"}`, rlTok, rlC3, rlC3), owner, 0, true)
	call(t, &ct, rlC3, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
		rlTok, rlC2), owner, 0, true)
	call(t, &ct, rlC3, "addChannel", fmt.Sprintf(
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"2",`+
			`"reporterAuth":"hive:rep1,hive:rep2,hive:rep3","reporterThreshold":"2","policy":"%s"}`, rlPol),
		owner, 0, true)
	call(t, &ct, rlC3, "addChannel", fmt.Sprintf(
		`{"channel":"lp","bucket":"lp","window":"1","reporterMode":"2",`+
			`"reporterAuth":"hive:lp1,hive:lp2,hive:lp3","reporterThreshold":"2","policy":"%s"}`, rlPol),
		owner, 0, true)
	call(t, &ct, rlTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, rlC2), owner, 0, true)
	call(t, &ct, rlC2, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, rlC3, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:anyone", 1, true)

	const action = "sr:author:0"
	root := func(c string) string {
		return `{"channel":"author","epoch":"0","root":"` + strings.Repeat(c, 64) +
			`","totalShares":"100","policy":"` + rlPol + `"}`
	}
	// three authorities, three different roots: the stalled-disagreement case
	caCall(t, &ct, rlC3, "submitRoot", root("a"), []string{"hive:rep1"}, 100, true)
	caCall(t, &ct, rlC3, "submitRoot", root("b"), []string{"hive:rep2"}, 100, true)
	caCall(t, &ct, rlC3, "submitRoot", root("c"), []string{"hive:rep3"}, 100, true)
	for _, a := range []string{"hive:rep1", "hive:rep2", "hive:rep3"} {
		t.Logf("BEFORE aseen %-10s = %q", a, rlState(t, &ct, "rep|aseen|"+action+"|"+a))
	}

	// Naming the LP channel while releasing an author-channel action must be refused
	// outright — the authority sets differ, so this could only ever clear the wrong
	// markers.
	r := caCall(t, &ct, rlC3, "releaseStaleAttest",
		`{"role":"rep","channel":"lp","action":"`+action+`"}`, []string{"hive:mallory"}, 100+28800, false)
	caFailedFor(t, r, "does not belong to channel")

	// the round must be untouched: every voter still recorded, the round still live
	for _, a := range []string{"hive:rep1", "hive:rep2", "hive:rep3"} {
		assert.Equal(t, "1", rlState(t, &ct, "rep|aseen|"+action+"|"+a),
			"a refused release must leave "+a+"'s vote marker in place")
	}
	assert.NotEmpty(t, rlState(t, &ct, "rep|astart|"+action),
		"a refused release must leave the round's start stamp in place")

	// and the CORRECT channel still releases it, which is what the entrypoint is for
	ok := caCall(t, &ct, rlC3, "releaseStaleAttest",
		`{"role":"rep","channel":"author","action":"`+action+`"}`, []string{"hive:anyone"}, 100+28800, true)
	assert.Contains(t, ok.Ret, `"released":"3"`)
	for _, a := range []string{"hive:rep1", "hive:rep2", "hive:rep3"} {
		assert.Empty(t, rlState(t, &ct, "rep|aseen|"+action+"|"+a),
			"a correct release must clear "+a+"'s marker so the round can be re-run")
	}

	// the whole point: after a correct release the authorities can vote again and the
	// epoch can still be finalized
	caCall(t, &ct, rlC3, "submitRoot", root("a"), []string{"hive:rep1"}, 100+28801, true)
	caCall(t, &ct, rlC3, "submitRoot", root("a"), []string{"hive:rep2"}, 100+28801, true)
	assert.Equal(t, strings.Repeat("a", 64), rlState(t, &ct, "root|author|0"),
		"the re-run round must be able to commit a root")
}
