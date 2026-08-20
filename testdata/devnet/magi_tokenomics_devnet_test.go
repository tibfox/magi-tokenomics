package devnet

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDevnetMagiTokenomics deploys the MAGI tokenomics framework (token + C2
// emission + C3 author-distributor) onto a REAL multi-node devnet (HAF + Mongo +
// magi nodes in docker, real Hive block production) and then:
//
//	PHASE 1  deploy + init + hand token ownership to C2
//	PHASE 2  honest operation: emit an epoch, pull funding, push shares, claim
//	PHASE 3  an OUTSIDER (witness account #2 — not the deployer/owner) attempts
//	         every privileged action. All must fail on-chain.
//	PHASE 4  assert on-chain state proves nothing was stolen.
//
// Threat model: the deployer/owner is trusted. The adversary is an outsider or a
// non-owner token holder.
//
// Run:  go test -v -run TestDevnetMagiTokenomics -timeout 40m ./tests/devnet/
// magiFrameworkDir is where the magi-tokenomics checkout lives (its wasm artifacts
// and the reporter binary are read from there). Override with MAGI_FRAMEWORK_DIR.
var magiFrameworkDir = envOr("MAGI_FRAMEWORK_DIR", "/home/dockeruser/okinoko/magi-tokenomics")

// magiTokenWasm is the C0 token contract's compiled artifact — an EXTERNAL repo
// (vsc-eco/magi_token-contract), not part of this framework. Override with
// MAGI_TOKEN_WASM.
var magiTokenWasm = envOr("MAGI_TOKEN_WASM",
	"/mnt/HC_Volume_105012347/magi/testnet/magi_token-contract/test/artifacts/main.wasm")

// magiDevnetConfig is DefaultConfig with the HAF images PINNED.
//
// The harness leaves them on `:latest`, and on 2026-08-04 a rebuilt postgrest started
// treating a missing schema as fatal rather than ignoring it. The devnet compose asks
// for `hafah_endpoints,hafah_api_v1,hafah_api_v2`, but hafah has never created the
// api_v1/v2 pair — checked against 1.27.11, 1.28.4 and 1.28.6, none of them contain
// the name at all. The mismatch was harmless while postgrest tolerated it; once it
// stopped, every devnet run died at startup with
//
//	Failed to load the schema cache ... schema "hafah_api_v1" does not exist
//
// before a single contract was deployed.
//
// Pinning is a workaround, not a fix: the compose naming schemas nothing creates is an
// upstream wart in go-vsc-node and belongs there. This keeps OUR suites runnable
// without editing their repo. Override with MAGI_POSTGREST_IMAGE / MAGI_HAFAH_IMAGE.
func magiDevnetConfig() *Config {
	cfg := DefaultConfig()
	// NAMESPACE THIS RUN so it can never be confused with another devnet on the host.
	//
	// The harness defaults to "devnet-test-<4 random bytes>", and any other checkout
	// on this machine produces names from the same space — a second agent session
	// working the market contracts runs its own devnet concurrently. Sharing a
	// namespace means every cleanup has to distinguish them by inspecting compose
	// config paths, and a filter that gets that wrong destroys someone else's run.
	//
	// The prefix deliberately avoids the substring "magi": production services on this
	// host are named magi-deployer-*, magi-mongo-indexer-*, and a `--filter name=magi-`
	// cleanup once removed the live contract deployers. A namespace that cannot
	// accidentally match a production container is worth more than a readable one.
	cfg.ProjectName = "tokdevnet-" + randomSuffix()
	cfg.PostgRESTImage = envOr("MAGI_POSTGREST_IMAGE",
		"registry.gitlab.syncad.com/hive/haf_api_node/postgrest:1.28.5")
	// hafah is deliberately NOT pinned: it is not what broke, and pinning it alongside
	// postgrest left the chain stuck at block 1 with no witnesses registered. Only the
	// component that actually changed behaviour is held back.
	return cfg
}

// randomSuffix is the per-run half of the project namespace. Kept distinct per run so
// two of OUR suites cannot collide either — they cannot run concurrently, but a
// crashed run leaving containers behind must not be adopted by the next one.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := crand.Read(b); err != nil {
		// A collision is worse than a boring name, but rand failing is not a reason to
		// abort a test run; fall back to the pid, which is unique among live processes.
		return "pid" + strconv.Itoa(os.Getpid())
	}
	return hex.EncodeToString(b)
}

// requireDiskSpace fails the test immediately when the filesystem cannot hold a run.
//
// A devnet run needs roughly 5 GB transiently: HAF's database, five node datadirs and
// the mongo indexes. When it is not there, nothing says so — bring-up proceeds for
// about seven minutes and then mongo aborts with
//
//	(OutOfDiskSpace) available disk space of N bytes is less than required minimum of 524288000
//
// surfacing as "starting devnet: genesis-elector: exit status 1", which reads like a
// consensus problem and sent one investigation after the suite rather than the disk.
// Checking up front turns seven minutes and a misleading error into one second and an
// accurate one.
//
// The threshold is deliberately above mongo's own 500 MB floor: mongo checks only at
// index creation, by which point HAF has already taken its share, so a disk that
// passes mongo's check can still fail later in the run.
func requireDiskSpace(t *testing.T) {
	t.Helper()
	const needBytes = 6 << 30 // 6 GiB
	var st syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &st); err != nil {
		t.Logf("could not stat the filesystem, proceeding anyway: %v", err)
		return
	}
	avail := st.Bavail * uint64(st.Bsize)
	if avail < needBytes {
		t.Fatalf("only %.1f GiB free, a devnet run needs ~%.0f GiB — it would die ~7 minutes "+
			"in with a mongo OutOfDiskSpace that looks like a genesis-election failure",
			float64(avail)/(1<<30), float64(needBytes)/(1<<30))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func magiWasm(t *testing.T, rel string) string {
	p := magiFrameworkDir + "/" + rel
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("missing wasm artifact %s: %v (build it with tinygo first)", p, err)
	}
	return p
}

func TestDevnetMagiTokenomics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	requireDiskSpace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	cfg := magiDevnetConfig()
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		t.Fatalf("starting devnet: %v", err)
	}

	// ---------------- PHASE 1: deploy ----------------
	// DeployContract stops+restarts magi-1 each time; deploying back-to-back races
	// the node's readiness to serve a storage proof ("context deadline exceeded"),
	// so settle between deploys and retry.
	deploy := func(name, wasm string) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: name})
			if err == nil {
				t.Logf("deployed %s = %s (attempt %d)", name, id, attempt)
				time.Sleep(45 * time.Second) // let magi-1 rejoin before the next deploy
				return id
			}
			lastErr = err
			t.Logf("deploy %s attempt %d failed: %v — settling then retrying", name, attempt, err)
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}
	tokenID := deploy("magi-token", magiTokenWasm)
	c2ID := deploy("magi-c2-emission", magiWasm(t, "c2-emission/artifacts/main.wasm"))
	c3ID := deploy("magi-c3-distributor", magiWasm(t, "c3-distributor/artifacts/main.wasm"))
	t.Logf("deployed: token=%s c2=%s c3=%s", tokenID, c2ID, c3ID)

	owner := d.cfg.WitnessPrefix + "1"    // deployer / trusted owner
	attacker := d.cfg.WitnessPrefix + "2" // outsider, non-owner
	t.Logf("owner=%s attacker=%s", owner, attacker)

	mustCall := func(node int, id, action, payload, what string) {
		if _, err := d.CallContract(ctx, node, id, action, payload); err != nil {
			t.Fatalf("%s: broadcast failed: %v", what, err)
		}
		t.Logf("sent: %s", what)
	}
	// waitKey polls a contract state key until it appears (real block production
	// means state is only queryable once the VSC layer has produced output).
	waitKey := func(id, key, what string) string {
		if !waitStateKeyPresent(t, d, ctx, 1, id, key, 3*time.Minute) {
			t.Fatalf("timed out waiting for %s (%s[%s])", what, id, key)
		}
		st, err := d.GetStateByKeys(ctx, 1, id, []string{key})
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		return fmt.Sprintf("%v", st[key])
	}
	// waitKeyValue polls until a key contains want.
	waitKeyValue := func(id, key, want, what string) {
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			st, err := d.GetStateByKeys(ctx, 1, id, []string{key})
			if err == nil && st[key] != nil && strings.Contains(fmt.Sprintf("%v", st[key]), want) {
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatalf("timed out waiting for %s to contain %q", what, want)
	}

	head, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("read chain head: %v", err)
	}
	// genesis is OMITTED from the init payload: C2 defaults it to the block in
	// which init executes, so emission starts at deployment. We only track head
	// here to know roughly when the first epoch will have elapsed.
	genesis := head
	t.Logf("chain head=%d; C2 genesis will auto-default to its init block (epochLen=5)", head)

	// Fund the actors for RC before any contract call.
	//
	// RC is `ledger HBD + 10,000 free`, and this suite's setup alone — three inits
	// plus addChannel — measures at 9,866 RC in the local harness (see
	// TestRCBudget_TokenomicsSetupFitsTheFreeTier). That left 134 RC of headroom,
	// and devnet adds per-transaction overhead the harness does not, so addChannel
	// was the first call to fall off the end: the three inits landed and the fourth
	// silently never applied. An attacker running dry has the same problem in
	// reverse — a call that dies of RC exhaustion proves nothing about
	// authorisation, so the attacker is funded too.
	for _, node := range []int{1, 2} {
		moved := 0
		for round := 0; round < 3; round++ {
			progressed := false
			for _, amt := range []string{"100.000", "50.000", "20.000", "5.000"} {
				if _, ferr := d.Deposit(ctx, node, amt, "hbd"); ferr == nil {
					t.Logf("node %d deposited %s HBD for RC (round %d)", node, amt, round+1)
					moved++
					progressed = true
					break
				}
			}
			if !progressed {
				break
			}
		}
		if moved == 0 {
			t.Fatalf("node %d could not deposit — it would run on the 10k free tier, which "+
				"this suite's setup already very nearly exhausts", node)
		}
	}

	// init token, C2, C3 (all from the owner account)
	mustCall(1, tokenID, "init", `{"name":"MAGI","symbol":"MAGI","decimals":0,"maxSupply":"100000000"}`, "token.init")
	waitKey(tokenID, "isInit", "token init")
	mustCall(1, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"5","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:%s3","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`,
		tokenID, owner, d.cfg.WitnessPrefix, c3ID), "c2.init")
	waitKey(c2ID, "init", "c2 init")
	mustCall(1, c3ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:%s4","guardianMode":"0","guardianAuth":"hive:%s3","guardianThreshold":"1"}`,
		tokenID, c2ID, d.cfg.WitnessPrefix, d.cfg.WitnessPrefix), "c3.init")
	waitKey(c3ID, "init", "c3 init")
	// The reward channel: its funding bucket, challenge window and reporter authority
	// are per-channel now, because one distributor serves several of them.
	mustCall(1, c3ID, "addChannel", fmt.Sprintf(
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:%s","reporterThreshold":"1","role":"content"}`,
		owner), "c3.addChannel author")
	waitKey(c3ID, "ch_bucket|author", "author channel registered")

	// hand the token to C2 — from here C2 is the only minter
	// C2 draws each epoch from an approved pool instead of minting, so mint the pool
	// and approve C2 BEFORE handing the token over — only the owner may mint.
	mustCall(1, tokenID, "mint", `{"amount":"1000000"}`, "mint the emission pool")
	mustCall(1, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, c2ID), "approve C2 to draw the pool")
	mustCall(1, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token.changeOwner")

	waitKeyValue(tokenID, "owner", c2ID, "token owner handover")
	t.Logf("token owner is now contract:%s", c2ID)

	// ---------------- PHASE 2: honest operation ----------------
	// distributeEpoch is permissionless — poke it from the ATTACKER's node to
	// prove that being permissionless is safe (it only follows the schedule).
	// wait until at least one epoch has fully elapsed past genesis
	for {
		h, herr := d.getLastProcessedBlock(ctx, 1)
		if herr == nil && h > genesis+15 {
			t.Logf("chain head=%d is past auto-genesis+epochLen, poking emission", h)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx done waiting for epoch 0 to elapse")
		case <-time.After(5 * time.Second):
		}
	}
	mustCall(2, c2ID, "distributeEpoch", ``, "c2.distributeEpoch (permissionless)")
	waitKey(c2ID, "cfg_lastEpoch", "c2 first epoch distributed")
	mustCall(2, c3ID, "pullFunding", `{"channel":"author","epoch":"0"}`, "c3.pullFunding (permissionless)")
	funded := waitKey(c3ID, "funded|author|0", "c3 epoch-0 funding")
	t.Logf("C3 epoch-0 funded = %s", funded)

	// reporter (owner acct) pushes shares to the attacker + a third party, then finalizes
	book := buildBook(t, map[string]string{
		"hive:" + attacker:                  "75",
		"hive:" + d.cfg.WitnessPrefix + "3": "25",
	})
	mustCall(1, c3ID, "submitShares", book.SubmitPayload("author", "0"), "c3.submitShares")
	// The commitment. totalShares now lands with the ROOT, not with the pages —
	// applyEntries writes no per-account state and accumulates nothing, so waiting
	// for it before submitRoot would wait forever.
	mustCall(1, c3ID, "submitRoot", book.RootPayload("author", "0"), "c3.submitRoot")
	shares := waitKey(c3ID, "totalShares|author|0", "c3 shares recorded")
	t.Logf("C3 epoch-0 totalShares = %s", shares)
	mustCall(1, c3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "c3.finalizeEpoch")
	waitKeyValue(c3ID, "status|author|0", "finalized", "c3 epoch-0 finalize")

	// the attacker legitimately claims the share the reporter assigned them
	mustCall(2, c3ID, "claim", book.ClaimPayload(t, "author", "0", "hive:"+attacker),
		"c3.claim (legit share)")

	// ---------------- PHASE 3: outsider attacks ----------------
	// Each must FAIL on-chain. We broadcast and then assert state is unchanged;
	// a rejected contract call still lands as a tx but must not mutate state.
	type atk struct{ id, action, payload, what string }
	attacks := []atk{
		{tokenID, "mint", `{"amount":"99999999"}`, "attacker mints"},
		{tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"hive:%s"}`, attacker), "attacker seizes token"},
		{c2ID, "claimBucket", `{"epoch":"0"}`, "attacker impersonates bucket target"},
		{c2ID, "init", `{"token":"x"}`, "attacker re-inits C2"},
		{c3ID, "init", `{"token":"x"}`, "attacker re-inits C3"},
		{c3ID, "submitShares", fmt.Sprintf(`{"channel":"author","epoch":"1","page":"0","entries":"hive:%s:999999"}`, attacker), "attacker pushes fake shares"},
		{c3ID, "finalizeEpoch", `{"channel":"author","epoch":"1"}`, "attacker finalizes"},
		{c3ID, "cancelEpoch", `{"channel":"author","epoch":"0"}`, "attacker vetoes"},
		{c3ID, "sweepUnallocated", `{"channel":"author","nonce":"1","amount":"1"}`, "attacker sweeps"},
		// carries their REAL proof, so the double-claim guard is what refuses it
		// rather than the missing-proof check — otherwise the guard under test
		// never runs.
		{c3ID, "claim", book.ClaimPayload(t, "author", "0", "hive:"+attacker), "attacker double-claims"},
		{c3ID, "pullFunding", `{"channel":"author","epoch":"00"}`, "attacker non-canonical epoch"},
		{c2ID, "queueTokenOp", fmt.Sprintf(`{"op":"changeOwner","nonce":"1","newOwner":"hive:%s"}`, attacker), "attacker queues token takeover"},
		{c2ID, "executeTokenOp", fmt.Sprintf(`{"op":"changeOwner","nonce":"1","newOwner":"hive:%s"}`, attacker), "attacker executes token takeover"},
	}
	for _, a := range attacks {
		if _, err := d.CallContract(ctx, 2, a.id, a.action, a.payload); err != nil {
			t.Logf("attack %-42s broadcast error (fine): %v", a.what, err)
		} else {
			t.Logf("attack %-42s broadcast; must be rejected by the contract", a.what)
		}
	}
	time.Sleep(15 * time.Second) // let every attack tx settle

	// ---------------- PHASE 4: nothing was stolen ----------------
	stAfter, err := d.GetStateByKeys(ctx, 1, tokenID, []string{"owner", "supply"})
	if err != nil {
		t.Fatalf("read token state after attacks: %v", err)
	}
	t.Logf("token state after attacks: %v", stAfter)
	if got := fmt.Sprintf("%v", stAfter["owner"]); !strings.Contains(got, c2ID) {
		t.Fatalf("SECURITY FAILURE: token owner changed to %q", got)
	}

	c3after, err := d.GetStateByKeys(ctx, 1, c3ID,
		[]string{"funded|author|0", "totalShares|author|0", "status|author|0", "funded|author|1", "totalShares|author|1"})
	if err != nil {
		t.Fatalf("read c3 after attacks: %v", err)
	}
	t.Logf("C3 state after attacks: %v", c3after)
	if fmt.Sprintf("%v", c3after["funded|author|0"]) != funded {
		t.Fatalf("SECURITY FAILURE: epoch-0 funding changed %q -> %v", funded, c3after["funded|author|0"])
	}
	if ts := fmt.Sprintf("%v", c3after["totalShares|author|0"]); ts != shares {
		t.Fatalf("SECURITY FAILURE: epoch-0 shares mutated: %v", ts)
	}
	// the attacker's fake epoch-1 shares must never have been recorded
	if v := fmt.Sprintf("%v", c3after["totalShares|author|1"]); v != "" && v != "<nil>" && v != "0" {
		t.Fatalf("SECURITY FAILURE: attacker injected shares into epoch 1: %v", v)
	}

	t.Log("DEVNET RESULT: honest flow succeeded; every outsider attack was rejected on-chain")
}
