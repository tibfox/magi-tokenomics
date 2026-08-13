package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestDevnetMagiScale runs a REALISTIC tribe for one epoch, configured exactly like a
// live Hive-Engine reward pool, at a load a real one produces:
//
//	500 users with stakes spanning four orders of magnitude
//	200 posts in the epoch
//	 50 votes on each post  ->  10,000 votes
//
// The other suites prove the mechanisms. This one asks a different question: does the
// pipeline survive the volume, what does an epoch COST, and does anyone get silently
// dropped when 500 earners have to be paginated onto the chain?
//
// SETTINGS, mapped from a live pool's admin panel:
//
//	post curve r^1, curation curve r^1     -> author_curve "1/1", curation_curve "1/1"
//	curation 50%                           -> author_reward_bps 5000
//	staked reward 50%                      -> C3 stakedBps 5000 + stakeContract
//	cashout 7 days                         -> source.cashout_days 7
//	5 tags, no exclusions                  -> source.tags (5), excluded_tags []
//	downvotes disabled                     -> source.disable_downvotes true
//	declined payouts honoured              -> source.ignore_declined_payout false
//	app tax off, reward reduction off      -> no app_tax; emission is flat by design
//	2 tokens / 120s = 1,440/day            -> one epoch here funds a WEEK: 10,080
//
// ★ WEIGHTING. A panel that carries vote-power settings implies SCOT weighs votes by
// the tribe's own staked token. That maps to weight=token_stake, and it is NOT what
// this suite uses: 500 on-chain stakes would cost ~185,000 RC of airdropBatch from a
// single owner account, well past what one devnet account can hold. The 500 users'
// differing stakes are expressed as differing rshares instead, which exercises the
// same share arithmetic and the same pagination. Vote mana is covered in-process by
// TestParams_Mana* in reporter/hivesrc.
//
// Run: go test -v -run TestDevnetMagiScale -timeout 60m ./tests/devnet/
const (
	scaleUsers      = 500
	scalePosts      = 200
	scaleVotesPost  = 50
	scaleEpochLen   = 20     // blocks; 60s, so the run is minutes not a week
	scaleEmission   = 10080  // one epoch funds a week at 1,440/day
	scaleBlocksYear = 1000   // with baseAnnual below: 504000*20/1000 = 10080
	scaleBaseAnnual = 504000
)

// scaleTags are the pool's five indexed tags. A post carries exactly one of them, so
// the reporter's five feed walks each return a real slice of the epoch.
var scaleTags = []string{"bbh", "inleo", "drip", "tip", "passive"}

// scaleFixture serves the injected Hive state. Unlike the smaller reporter fixture it
// PAGINATES properly, 20 posts at a time per tag, because a 200-post epoch is exactly
// where a walk that terminates early would silently drop earners.
type scaleFixture struct {
	head      uint64
	genesis   uint64
	epochLen  uint64
	blockTime time.Time
	byTag     map[string][]map[string]any
	votes     map[string][]map[string]any
	hits      map[string]int
}

// scaleUser is a synthetic tribe member. Weight spans four orders of magnitude so the
// share book has whales, a long tail, and dust that must not round anyone to zero
// silently.
func scaleUserBare(i int) string { return fmt.Sprintf("u%03d", i) }

// scaleWeight gives user i a vote weight. Deterministic, no randomness: a devnet run
// that cannot be reproduced is not evidence of anything.
//
//	i%50 == 0  -> whale        ~1e15
//	i%10 == 0  -> large        ~1e13
//	i%3  == 0  -> medium       ~1e11
//	otherwise  -> long tail    ~1e9
func scaleWeight(i int) int64 {
	switch {
	case i%50 == 0:
		return 1_000_000_000_000_000 + int64(i)*1_000_000_000
	case i%10 == 0:
		return 10_000_000_000_000 + int64(i)*1_000_000_000
	case i%3 == 0:
		return 100_000_000_000 + int64(i)*1_000_000
	default:
		return 1_000_000_000 + int64(i)*1_000
	}
}

// buildScaleFixture lays out one epoch: scalePosts posts spread across the five tags,
// each with scaleVotesPost votes drawn from the user population.
//
// w1/w2 are REAL devnet witness accounts, placed deliberately as an author and as a
// curator on many posts. Only they hold keys, so only they can exercise the claim
// path — a synthetic name can earn a share and never collect it.
func buildScaleFixture(genesis, epochLen uint64, blockTime time.Time, w1, w2 string) *scaleFixture {
	f := &scaleFixture{
		head: genesis + 3*epochLen, genesis: genesis, epochLen: epochLen,
		blockTime: blockTime,
		byTag:     map[string][]map[string]any{},
		votes:     map[string][]map[string]any{},
		hits:      map[string]int{},
	}
	const payoutPeriod = 7 * 24 * time.Hour
	hivefmt := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") }

	for p := 0; p < scalePosts; p++ {
		tag := scaleTags[p%len(scaleTags)]
		// spread payouts across the epoch's blocks so membership is decided by a
		// genuine window rather than every post sharing one instant
		offset := uint64(p) * epochLen / scalePosts
		payout := blockTime.Add(time.Duration(offset) * 3 * time.Second)
		created := payout.Add(-payoutPeriod)

		// every 20th post is authored by a real witness so the claim path has an
		// author to test; the rest come from the synthetic population
		author := scaleUserBare(p % scaleUsers)
		if p%20 == 0 {
			author = w1
		}
		permlink := fmt.Sprintf("post-%03d", p)

		f.byTag[tag] = append(f.byTag[tag], map[string]any{
			"author": author, "permlink": permlink, "depth": 0,
			"category":   tag,
			"created":    hivefmt(created),
			"payout_at":  hivefmt(payout),
			"is_paidout": true,
			"stats":      map[string]any{"is_pinned": false},
			// author-controlled metadata: the tag list and the publishing app
			"json_metadata":       fmt.Sprintf(`{"tags":["%s"],"app":"peakd/2023.1"}`, tag),
			"max_accepted_payout": "1000000.000 HBD",
		})

		vs := make([]map[string]any, 0, scaleVotesPost)
		for v := 0; v < scaleVotesPost; v++ {
			// stride through the population so each post draws a different slice
			ui := (p*7 + v*13) % scaleUsers
			voter := scaleUserBare(ui)
			if v == 0 {
				voter = w2 // a real witness curates every post
			}
			// votes land through the 7-day window, before the cutoff
			hrs := (v * 3) % 160
			vs = append(vs, map[string]any{
				"voter":   voter,
				"rshares": fmt.Sprint(scaleWeight(ui)),
				"percent": 10000,
				"time":    hivefmt(created.Add(time.Duration(hrs) * time.Hour)),
			})
		}
		// One downvote per tenth post. Downvotes are DISABLED in this configuration,
		// so these must have no effect at all — which is only meaningful if they are
		// actually present in the data.
		if p%10 == 0 {
			vs = append(vs, map[string]any{
				"voter":   scaleUserBare((p + 1) % scaleUsers),
				"rshares": "-5000000000000",
				"percent": -10000,
				"time":    hivefmt(created.Add(80 * time.Hour)),
			})
		}
		f.votes[author+"/"+permlink] = vs
	}
	return f
}

func (f *scaleFixture) totalPosts() int {
	n := 0
	for _, ps := range f.byTag {
		n += len(ps)
	}
	return n
}

// serve answers the Hive JSON-RPC calls the reporter makes, with REAL cursor
// pagination: 20 posts per page, per tag.
func (f *scaleFixture) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     int             `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.hits[req.Method]++
		var result any
		switch req.Method {
		case "condenser_api.get_dynamic_global_properties":
			result = map[string]any{"head_block_number": f.head}
		case "block_api.get_block_header":
			var p struct {
				BlockNum uint64 `json:"block_num"`
			}
			_ = json.Unmarshal(req.Params, &p)
			ts := f.blockTime.Add(time.Duration(int64(p.BlockNum)-int64(f.genesis)) * 3 * time.Second)
			result = map[string]any{"header": map[string]any{
				"timestamp": ts.UTC().Format("2006-01-02T15:04:05")}}
		case "bridge.get_ranked_posts":
			var p map[string]any
			_ = json.Unmarshal(req.Params, &p)
			tag, _ := p["tag"].(string)
			posts := f.byTag[tag]
			start := 0
			if sa, ok := p["start_author"].(string); ok {
				sp, _ := p["start_permlink"].(string)
				for i, post := range posts {
					if post["author"] == sa && post["permlink"] == sp {
						start = i + 1
						break
					}
				}
			}
			end := start + 20
			if end > len(posts) {
				end = len(posts)
			}
			if start >= len(posts) {
				result = []any{}
			} else {
				result = posts[start:end]
			}
		case "condenser_api.get_active_votes":
			var p []any
			_ = json.Unmarshal(req.Params, &p)
			key := fmt.Sprintf("%v/%v", p[0], p[1])
			if v, ok := f.votes[key]; ok {
				result = v
			} else {
				result = []any{}
			}
		default:
			http.Error(w, "unexpected method "+req.Method, 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": result,
		})
	}))
}

func TestDevnetMagiScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	requireDiskSpace(t)
	if _, err := os.Stat(reporterBin); err != nil {
		t.Fatalf("reporter binary missing at %s — build it first", reporterBin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
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

	w := func(n int) string { return d.cfg.WitnessPrefix + strconv.Itoa(n) }
	owner := w(1)    // deployer, reporter, and an author
	curator := w(2)  // curates every post
	treasury := w(4)
	guardian := w(5)

	// ---------------- PHASE 1: deploy ----------------
	deployN := func(name, wasm string, node int) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, err := d.DeployContract(ctx, ContractDeployOpts{
				WasmPath: wasm, Name: name, DeployerNode: node,
			})
			if err == nil {
				t.Logf("deployed %-18s = %s (node %d)", name, id, node)
				time.Sleep(45 * time.Second)
				return id
			}
			lastErr = err
			t.Logf("deploy %s attempt %d failed: %v — settling", name, attempt, err)
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}
	ownerNode := map[string]int{}
	dep := func(name, wasm string, node int) string {
		id := deployN(name, wasm, node)
		ownerNode[id] = node
		return id
	}
	// NOT node 1. Every deploy costs 10 HBD of the DEPLOYER's L1 balance, and L1
	// balance is what becomes RC — so deploying from the reporter's account spends
	// the capacity it needs for nine share pages. Node 1 reports and does nothing
	// else; the deploys are spread across two other funded accounts.
	tokenID := dep("magi-token", magiTokenWasm, 2)
	c2ID := dep("magi-c2-emission", magiWasm(t, "c2-emission/artifacts/main.wasm"), 2)
	c3ID := dep("magi-distributor", magiWasm(t, "c3-distributor/artifacts/main.wasm"), 3)
	c1ID := dep("magi-c1-staking", magiWasm(t, "c1-staking/artifacts/main.wasm"), 3)

	// The reporter carries the whole epoch: ~9 share pages at ~5,900 RC each. Fund it
	// as heavily as L1 allows or it runs out mid-epoch, which looks like a rejected
	// page rather than an empty wallet.
	for _, node := range []int{1, 2, 3, 4, 5} {
		moved := 0
		for round := 0; round < 4; round++ {
			progressed := false
			for _, amt := range []string{"200.000", "100.000", "50.000", "20.000", "5.000"} {
				if _, ferr := d.Deposit(ctx, node, amt, "hbd"); ferr == nil {
					t.Logf("node %d deposited %s HBD", node, amt)
					moved++
					progressed = true
					break
				}
			}
			if !progressed {
				break
			}
		}
		if moved == 0 && node <= 3 {
			t.Fatalf("node %d could not deposit — it would run on the 10k free tier alone", node)
		}
	}
	// POOL the idle accounts' balance into the reporter.
	//
	// RC capacity is the account's L2 HBD balance plus the 10,000 free tier, and one
	// witness can only deposit what it holds on L1 — about 100 HBD, i.e. ~110,000 RC.
	// A 500-earner epoch costs ~125,000: 502 entries at ~215 RC each is the floor, and
	// pagination barely moves it. Run 22 died exactly there, seven pages in, with
	// 109,667 of 110,000 spent.
	//
	// Nodes 4 and 5 appear in this suite only as a treasury and a guardian ADDRESS —
	// neither of them signs anything — so their balances are dead weight. Moving them
	// to the reporter is what makes an epoch this size affordable at all on devnet.
	time.Sleep(20 * time.Second) // let the deposits credit before moving them
	for _, from := range []int{4, 5} {
		if _, terr := d.Transfer(from, 1, "95.000", "hbd", "fund the reporter"); terr != nil {
			t.Logf("could not pool node %d's balance: %v", from, terr)
		} else {
			t.Logf("pooled 95 HBD from node %d into the reporter", from)
		}
	}

	deadline := time.Now().Add(4 * time.Minute)
	for {
		b, _ := d.GetAccountBalance(ctx, 1, "hive:"+owner)
		if b != nil && b.Hbd > 0 {
			t.Logf("reporter ledger hbd=%d -> RC ~%d", b.Hbd, b.Hbd+10000)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deposits never credited — the reporter cannot fund 9 share pages on the free tier")
		}
		time.Sleep(6 * time.Second)
	}

	// ---------------- helpers ----------------
	callN := func(node int, id, action, payload, what string) {
		if _, err := d.CallContract(ctx, node, id, action, payload); err != nil {
			t.Fatalf("%s: broadcast failed: %v", what, err)
		}
		t.Logf("sent: %s", what)
		time.Sleep(9 * time.Second)
	}
	callOwner := func(id, action, payload, what string) { callN(ownerNode[id], id, action, payload, what) }
	stateOf := func(id, key string) string {
		st, err := d.GetStateByKeys(ctx, 1, id, []string{key})
		if err != nil || st == nil || st[key] == nil {
			return ""
		}
		return fmt.Sprintf("%v", st[key])
	}
	waitKey := func(id, key, what string) string {
		if !waitStateKeyPresent(t, d, ctx, 1, id, key, 4*time.Minute) {
			t.Fatalf("timed out waiting for %s (%s[%s])", what, id, key)
		}
		return stateOf(id, key)
	}
	waitValue := func(id, key, want, what string) {
		dl := time.Now().Add(5 * time.Minute)
		for time.Now().Before(dl) {
			if stateOf(id, key) == want {
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatalf("%s: %s[%s] never reached %q (last %q)", what, id, key, want, stateOf(id, key))
	}
	initAs := func(id, payload, proofKey, what string) {
		callOwner(id, "init", payload, what)
		waitKey(id, proofKey, what+" (confirming)")
	}

	// ---------------- PHASE 2: init, wired exactly like the pool ----------------
	initAs(tokenID, `{"name":"Scale","symbol":"SCALE","decimals":0,"maxSupply":"1000000000"}`,
		"owner", "token init")
	// C1 first: the distributor cross-checks the token against it, and the allowlist
	// naming the distributor is fixed here forever.
	initAs(c1ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"%d","epochLen":"%d","allow":"contract:%s",`+
			`"treasury":"hive:%s","guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, scaleEpochLen*3, scaleEpochLen, c3ID, treasury, guardian), "cfg_token", "C1 init")

	callOwner(tokenID, "mint", `{"amount":"100000000"}`, "mint the emission pool")
	callOwner(tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"100000000"}`, c2ID), "approve C2 to draw the pool")
	callOwner(tokenID, "changeOwner",
		fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token ownership -> C2")

	// ONE reward pool, so ONE bucket taking the whole emission.
	initAs(c2ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"%d",`+
			`"blocksPerYear":"%d","dustBucket":"content","timelock":"60",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:10000"}`,
		tokenID, scaleEpochLen, scaleBaseAnnual, scaleBlocksYear, guardian, treasury, c3ID),
		"cfg_genesis", "C2 init")
	genesis, _ := strconv.ParseUint(waitKey(c2ID, "cfg_genesis", "C2 genesis"), 10, 64)
	t.Logf("C2 genesis=%d epochLen=%d -> epoch 0 = blocks %d..%d, funding %d",
		genesis, scaleEpochLen, genesis, genesis+scaleEpochLen-1, scaleEmission)

	// 50% of every claim arrives as STAKE — the panel's staked_reward_percentage.
	initAs(c3ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"stakeContract":"%s","stakedBps":"5000"}`,
		tokenID, c2ID, treasury, guardian, c1ID), "cfg_funder", "distributor init")
	callOwner(c3ID, "addChannel", fmt.Sprintf(
		`{"channel":"content","bucket":"content","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:%s","reporterThreshold":"1","role":"content"}`, owner),
		"addChannel content")
	waitKey(c3ID, "ch_bucket|content", "content channel registered")

	// ---------------- PHASE 3: one epoch of a busy tribe ----------------
	t.Logf("waiting for epoch 0 to close (block %d)...", genesis+scaleEpochLen)
	time.Sleep(time.Duration(scaleEpochLen+8) * 3 * time.Second)
	callN(1, c2ID, "distributeEpoch", `{}`, "keeper poke")
	waitValue(c2ID, "owed|content|0", strconv.Itoa(scaleEmission), "C2 owed[content]")
	callN(1, c3ID, "pullFunding", `{"channel":"content","epoch":"0"}`, "pull funding")
	waitValue(c3ID, "funded|content|0", strconv.Itoa(scaleEmission), "C3 funded")

	blockTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	fx := buildScaleFixture(genesis, scaleEpochLen, blockTime, owner, curator)
	t.Logf("fixture: %d posts across %d tags, %d votes each (%d votes total), %d users",
		fx.totalPosts(), len(scaleTags), scaleVotesPost, fx.totalPosts()*scaleVotesPost, scaleUsers)
	hive := fx.serve(t)
	defer hive.Close()

	// ---------------- PHASE 4: the real reporter, at load ----------------
	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "reporter.json")
	blob, _ := json.MarshalIndent(map[string]any{
		"hive":      map[string]any{"api": []string{hive.URL}},
		"vsc":       map[string]any{"api": d.GQLEndpoint(1), "net_id": "vsc-devnet"},
		"contracts": map[string]any{"distributor": c3ID, "channel": "content", "funder": c2ID, "stake": ""},
		"epoch":     map[string]any{"genesis": genesis, "len": scaleEpochLen},
		"source": map[string]any{
			"tags":                   scaleTags,
			"excluded_tags":          []string{},
			"limit":                  1000,
			"weight":                 "hive_rshares",
			"exclude":                []string{},
			"cashout_days":           7,
			"ignore_declined_payout": false,
			"disable_downvotes":      true,
		},
		"shares": map[string]any{
			"author_reward_bps": 5000,
			"author_curve":      "1/1",
			"curation_curve":    "1/1",
			"muted":             []string{},
			"staked_bps":        5000,
		},
		"page": map[string]any{"max_entries": 60, "max_bytes": 3800},
		// finalize:true puts finalizeEpoch in the PLAN. Without it the plan is share
		// pages only, and the epoch never reaches "finalized" — which surfaces as a
		// timeout waiting for a status key rather than as a missing call.
		"submit": map[string]any{"account": owner, "rc_limit": 30000, "finalize": true,
			"progress_file": filepath.Join(workDir, "progress.json")},
	}, "", "  ")
	if err := os.WriteFile(cfgPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	// Reuse the package's own plan/compute helpers rather than re-declaring the JSON
	// shapes: `plan` carries only {epoch, calls} and the totals come from `compute`.
	// Getting that wrong is what cost this suite its first devnet run.
	// Capture stderr. The reporter reports WHY it refuses on stderr and prints
	// nothing on stdout, so .Output() alone turns an explained refusal into a bare
	// "exit status 1" — and that costs a whole devnet run to re-learn.
	runReporter := func(args ...string) []byte { return scaleReporter(t, cfgPath, args...) }

	planStart := time.Now()
	var plan reporterPlan
	if err := json.Unmarshal(runReporter("plan", "-epoch", "0", "-json"), &plan); err != nil {
		t.Fatalf("reporter plan json: %v", err)
	}
	expected := reporterCompute(t, runReporter, "0")
	t.Logf("REPORTER: %d accounts, totalShares=%s, %d calls, computed in %s",
		expected.Accounts, expected.TotalShares, len(plan.Calls),
		time.Since(planStart).Round(time.Millisecond))

	if expected.Accounts < 400 {
		t.Fatalf("expected the epoch to pay hundreds of accounts, got %d — the walk is "+
			"dropping posts or the fixture is not being read", expected.Accounts)
	}

	// ---------------- PHASE 5: put 500 earners on chain ----------------
	// Send the share pages FIRST and confirm them, then finalize.
	//
	// finalizeEpoch is irreversible and the contract cannot tell a complete report
	// from a partial one — it only checks that SOMETHING was submitted. Broadcasting
	// the plan straight through means finalizing over whatever happened to have
	// landed, which pays the whole epoch to a fraction of the earners. The reporter's
	// own `run` gates on this for the same reason; a suite that broadcasts the plan
	// blindly has to do it by hand.
	type call struct{ id, action, payload string }
	pages := 0
	var finalizeCall call
	var sharePages []call
	for i, c := range plan.Calls {
		if c.Action == "finalizeEpoch" {
			finalizeCall = call{c.ContractID, c.Action, c.Payload}
			continue
		}
		sharePages = append(sharePages, call{c.ContractID, c.Action, c.Payload})
		pages++
		callN(1, c.ContractID, c.Action, c.Payload, fmt.Sprintf("plan[%d] %s", i, c.Action))
	}
	t.Logf("submitted %d share pages for %d accounts", pages, expected.Accounts)

	// Confirm each page INDIVIDUALLY and resend the ones that did not land.
	//
	// Run 20 lost roughly a fifth of the shares here — one page of nine, while the
	// pages after it applied — so it was not RC running out, which would have taken
	// everything downstream with it. A single page reverting on L2 after a successful
	// broadcast is a normal thing to survive, and `ssdone|<ch>|<ep>|<page>` is the
	// receipt the contract writes only on application, so it can be checked per page
	// instead of inferred from a total that is 80% of what it should be.
	//
	// submitShares is idempotent per (channel, epoch, page), so resending a page that
	// DID apply is refused harmlessly. That makes resend-the-missing safe and is
	// exactly what `reporter run` does.
	pageApplied := func(i int) bool {
		return stateOf(c3ID, fmt.Sprintf("ssdone|content|0|%d", i)) != ""
	}
	// WAIT before calling a page missing. Broadcasting is not executing: the last
	// page is only seconds old when the loop above ends, and a check taken right
	// then reports almost everything as unapplied. Resending on that basis is not
	// merely wasteful — nine pages cost ~75,000 RC and a needless resend of seven
	// costs another ~59,000, which overruns the reporter's capacity and manufactures
	// the very failure this is meant to survive.
	missingPages := func(deadline time.Duration) []int {
		dl := time.Now().Add(deadline)
		for {
			var missing []int
			for i := 0; i < pages; i++ {
				if !pageApplied(i) {
					missing = append(missing, i)
				}
			}
			if len(missing) == 0 || time.Now().After(dl) {
				return missing
			}
			time.Sleep(10 * time.Second)
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		missing := missingPages(4 * time.Minute)
		if len(missing) == 0 {
			break
		}
		t.Logf("attempt %d: pages not applied: %v — resending", attempt, missing)
		if attempt == 3 {
			t.Fatalf("pages %v never applied after 3 attempts; chain totalShares=%s want %s",
				missing, stateOf(c3ID, "totalShares|content|0"), expected.TotalShares)
		}
		for _, i := range missing {
			callN(1, sharePages[i].id, sharePages[i].action, sharePages[i].payload,
				fmt.Sprintf("resend page %d", i))
		}
		time.Sleep(20 * time.Second)
	}

	// Only now is the epoch complete enough to close.
	waitValue(c3ID, "totalShares|content|0", expected.TotalShares,
		"all share pages applied before finalize")
	t.Logf("all %d pages applied: totalShares=%s", pages, expected.TotalShares)

	if finalizeCall.id == "" {
		t.Fatal("the plan contained no finalizeEpoch — set submit.finalize")
	}
	callN(1, finalizeCall.id, finalizeCall.action, finalizeCall.payload, "finalizeEpoch")
	waitKey(c3ID, "status|content|0", "epoch finalized")
	waitValue(c3ID, "totalShares|content|0", expected.TotalShares, "chain totalShares")
	t.Logf("SEAM OK: chain totalShares == reporter totalShares == %s", expected.TotalShares)

	// ---------------- PHASE 6: claim, split 50/50 liquid and staked ----------------
	time.Sleep(20 * time.Second) // challenge window
	for _, acct := range []string{owner, curator} {
		share := stateOf(c3ID, "share|content|0|hive:"+acct)
		if share == "" || share == "0" {
			t.Fatalf("%s earned nothing — the witness accounts must be in earning "+
				"positions or the claim path cannot be tested", acct)
		}
		t.Logf("%s share=%s", acct, share)
	}
	callN(1, c3ID, "claim", `{"channel":"content","epoch":"0"}`, "owner claims")
	callN(2, c3ID, "claim", `{"channel":"content","epoch":"0"}`, "curator claims")

	for _, acct := range []string{owner, curator} {
		if !waitStateKeyPresent(t, d, ctx, 1, c3ID, "claimed|content|0|hive:"+acct, 4*time.Minute) {
			t.Fatalf("%s never claimed", acct)
		}
		// HALF of it must have arrived as stake, not liquid tokens
		st := stateOf(c1ID, "stake|hive:"+acct)
		if st == "" || st == "0" {
			t.Fatalf("%s claimed but holds no stake — the 50%% staked split did not "+
				"reach the staking contract", acct)
		}
		t.Logf("%s staked=%s (the other half went out liquid)", acct, st)
	}

	// The funding ledger is what every payout divides, so it must not have moved
	// while 9 pages and two claims landed against it.
	funded, _ := new(big.Int).SetString(stateOf(c3ID, "funded|content|0"), 10)
	if funded == nil || funded.Cmp(big.NewInt(scaleEmission)) != 0 {
		t.Fatalf("funding drifted during the epoch: want %d, got %v", scaleEmission, funded)
	}

	t.Logf("SCALE RUN OK: %d posts, %d votes, %d accounts, %d pages, %d funded",
		fx.totalPosts(), fx.totalPosts()*scaleVotesPost, expected.Accounts, pages, scaleEmission)
	t.Logf("hive rpc calls: %v", fx.hits)
}

// TestScalePreflight runs the ENTIRE reporter pipeline against the scale fixture with
// no devnet at all: 200 posts across five tags, 10,000 votes, share computation and
// pagination. It needs no docker and finishes in seconds.
//
// It exists because the expensive failures are cheap ones in disguise. A fixture that
// paginates wrong, a config key that moved, a walk that stops early — each costs
// twenty minutes to discover on a real chain and one second here. Run this before
// TestDevnetMagiScale, always.
// serveVSCStub answers just enough GraphQL for `reporter compute` to run off-chain.
//
// compute cross-checks its epoch schedule against the distributor's own cfg_genesis
// and cfg_epochLen before it will produce anything — deliberately, because a silent
// mismatch would report the wrong block range and then finalize it. The pre-flight
// therefore needs a distributor to agree with, and this is the smallest thing that
// can: four keys, no chain.
// scaleReporter runs the reporter binary the way the CLI expects: SUBCOMMAND first,
// -config as a flag on it. Both the pre-flight and the devnet run go through this,
// deliberately — the first version had the devnet suite build its own command with
// the flag first, which the CLI read as the command name. The pre-flight passed
// because it happened to build the same call correctly and separately, so it proved
// the pipeline while missing the harness that drives it. One helper, one chance to
// get it wrong, and the cheap test covers it.
func scaleReporter(t *testing.T, cfgPath string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(reporterBin, append(args, "-config", cfgPath)...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reporter %v failed: %v\nstdout: %s\nstderr: %s", args, err, out, errBuf.String())
	}
	return out
}

func serveVSCStub(genesis, epochLen uint64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"getStateByKeys": map[string]any{
					"cfg_genesis":  fmt.Sprint(genesis),
					"cfg_epochLen": fmt.Sprint(epochLen),
					"cfg_funder":   "",
					"cfg_role":     "content",
				},
			},
		})
	}))
}

func TestScalePreflight(t *testing.T) {
	if _, err := os.Stat(reporterBin); err != nil {
		t.Skipf("reporter binary missing at %s", reporterBin)
	}
	const genesis, epochLen = 1000, scaleEpochLen
	blockTime := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	fx := buildScaleFixture(genesis, epochLen, blockTime, "magi.test1", "magi.test2")
	if got := fx.totalPosts(); got != scalePosts {
		t.Fatalf("fixture built %d posts, want %d", got, scalePosts)
	}
	hive := fx.serve(t)
	defer hive.Close()
	vsc := serveVSCStub(genesis, epochLen)
	defer vsc.Close()

	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "reporter.json")
	blob, _ := json.MarshalIndent(map[string]any{
		"hive":      map[string]any{"api": []string{hive.URL}},
		"vsc":       map[string]any{"api": vsc.URL, "net_id": "vsc-devnet"},
		"contracts": map[string]any{"distributor": "vsc1BdaRpLsgEk49mxemZ19QLxN6WTrzLPuVQv",
			"channel": "content", "funder": "", "stake": ""},
		"epoch": map[string]any{"genesis": genesis, "len": epochLen},
		"source": map[string]any{
			"tags": scaleTags, "excluded_tags": []string{}, "limit": 1000,
			"weight": "hive_rshares", "exclude": []string{}, "cashout_days": 7,
			"ignore_declined_payout": false, "disable_downvotes": true,
		},
		"shares": map[string]any{
			"author_reward_bps": 5000, "author_curve": "1/1", "curation_curve": "1/1",
			"muted": []string{}, "staked_bps": 5000,
		},
		"page":   map[string]any{"max_entries": 60, "max_bytes": 3800},
		"submit": map[string]any{"account": "magi.test1", "rc_limit": 30000, "finalize": true},
	}, "", "  ")
	if err := os.WriteFile(cfgPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out := scaleReporter(t, cfgPath, "compute", "-epoch", "0", "-json")
	var res struct {
		Posts       int    `json:"posts"`
		Accounts    int    `json:"accounts"`
		TotalShares string `json:"total_shares"`
		Pages       []struct {
			Index   int    `json:"Index"`
			Entries string `json:"Entries"`
			Count   int    `json:"Count"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("compute json: %v\n%s", err, out)
	}
	elapsed := time.Since(start)

	t.Logf("PREFLIGHT: %d posts, %d earners, totalShares=%s, %d pages, %s",
		res.Posts, res.Accounts, res.TotalShares, len(res.Pages), elapsed.Round(time.Millisecond))
	t.Logf("hive rpc calls: %v", fx.hits)

	// Every post in the epoch must be seen. A short walk is the failure that pays
	// some earners and silently drops the rest.
	if res.Posts != scalePosts {
		t.Fatalf("reporter saw %d posts, fixture served %d — the five-tag walk is "+
			"dropping some", res.Posts, scalePosts)
	}
	if res.Accounts < 400 {
		t.Fatalf("only %d earners from %d users — shares are collapsing", res.Accounts, scaleUsers)
	}
	if len(res.Pages) < 5 {
		t.Fatalf("%d earners should need several pages, got %d", res.Accounts, len(res.Pages))
	}
	// One get_active_votes per post, and no more: re-fetching votes per TAG would
	// multiply the load by five on a real node.
	if got := fx.hits["condenser_api.get_active_votes"]; got != scalePosts {
		t.Fatalf("votes fetched %d times for %d posts — a post reachable under several "+
			"tags must be collected once", got, scalePosts)
	}

	// PARSE THE PLAN TOO. compute and plan emit DIFFERENT shapes — plan carries only
	// {epoch, calls} and epoch is a STRING — and checking only compute is what let a
	// wrong plan struct survive to a devnet run and fail thirteen minutes in. The
	// call list is what actually gets broadcast, so it is the half worth validating.
	pout := scaleReporter(t, cfgPath, "plan", "-epoch", "0", "-json")
	var plan reporterPlan
	if err := json.Unmarshal(pout, &plan); err != nil {
		t.Fatalf("plan json does not match reporterPlan: %v\n%s", err, pout)
	}
	shares := 0
	for _, c := range plan.Calls {
		if c.Action == "submitShares" {
			shares++
		}
		if c.ContractID == "" || c.Payload == "" {
			t.Fatalf("plan call %q has an empty contract or payload — it would broadcast nothing", c.Action)
		}
	}
	if shares != len(res.Pages) {
		t.Fatalf("plan has %d submitShares calls for %d computed pages", shares, len(res.Pages))
	}
	// The epoch has to be CLOSED by the plan, or the devnet run waits forever on a
	// status key that nothing writes.
	if len(plan.Calls) != shares+1 || plan.Calls[len(plan.Calls)-1].Action != "finalizeEpoch" {
		t.Fatalf("the plan must end in finalizeEpoch, got %d calls ending in %q",
			len(plan.Calls), plan.Calls[len(plan.Calls)-1].Action)
	}
	t.Logf("PLAN: %d calls (%d share pages) for epoch %s", len(plan.Calls), shares, plan.Epoch)
}
