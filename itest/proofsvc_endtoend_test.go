package itest_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"magi_token/indexer/proofsvc"
	"magi_token/reporter/sharecore"
)

// The full loop, with no step simulated: the contract publishes a share book and
// commits a root, the indexer's rebuilder reads the contract's OWN emitted logs,
// and a claimant pays out using the payload that rebuilder served — verified by
// the contract's wasm verifier, not by Go.
//
// Each half was already tested against the other's assumptions rather than against
// the other. proofsvc's tests check its proofs with sharecore.VerifyProof, which is
// the same Go code that built them, so a construction both halves got wrong would
// pass. This is the test that cannot: the bytes come from the chain and the verdict
// comes from the chain.

// rowsFromLogs turns the distributor's emitted logs into the rows the indexer's
// mappings produce. This is the mapping's contract, exercised: if a field is
// renamed in the contract and not in the YAML, the rebuild here stops working for
// the same reason the real indexer would.
func rowsFromLogs(t *testing.T, logs []string) ([]proofsvc.SharesRow, []proofsvc.RootRow) {
	t.Helper()
	var pages []proofsvc.SharesRow
	var roots []proofsvc.RootRow
	for _, raw := range logs {
		var top struct {
			Type  string            `json:"type"`
			Attrs map[string]string `json:"attributes"`
		}
		if err := json.Unmarshal([]byte(raw), &top); err != nil {
			continue
		}
		num := func(k string) int64 {
			v, _ := strconv.ParseInt(top.Attrs[k], 10, 64)
			return v
		}
		switch top.Type {
		case "shares":
			pages = append(pages, proofsvc.SharesRow{
				Channel: top.Attrs["channel"], Epoch: num("epoch"), Page: num("page"),
				Entries: num("entries"), PageTotal: proofsvc.Numeric(top.Attrs["page_total"]),
				Submitted: top.Attrs["submitted"],
			})
		case "root":
			roots = append(roots, proofsvc.RootRow{
				Channel: top.Attrs["channel"], Epoch: num("epoch"), Root: top.Attrs["root"],
				TotalShares: proofsvc.Numeric(top.Attrs["total_shares"]), Accounts: num("accounts"),
			})
		}
	}
	return pages, roots
}

func TestIndexerRebuildsFromChainLogsAndItsProofPaysOut(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(12)

	// publish in two pages so the rebuild has to reassemble, not just parse one row
	b := shareBookBig(shares)
	logs := []string{}
	perPage := (len(b.tree.Leaves) + 1) / 2
	page := 0
	for i := 0; i < len(b.tree.Leaves); i += perPage {
		end := min(i+perPage, len(b.tree.Leaves))
		var sb strings.Builder
		for j := i; j < end; j++ {
			if j > i {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "%s:%s", b.tree.Leaves[j].Account, b.tree.Leaves[j].Share)
		}
		logs = append(logs, collectLogs(t, ct, mkDist, "submitShares", fmt.Sprintf(
			`{"channel":"content","epoch":"0","page":"%d","entries":"%s"}`, page, sb.String()),
			"hive:creporter", 1)...)
		page++
	}
	logs = append(logs, collectLogs(t, ct, mkDist, "submitRoot", fmt.Sprintf(
		`{"channel":"content","epoch":"0","root":"%s","totalShares":"%s","accounts":"%d"}`,
		b.tree.Root(), b.total.String(), len(b.tree.Leaves)), "hive:creporter", 1)...)
	call(t, ct, mkDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:creporter", 1, true)

	// the indexer's view, built from what the chain actually emitted
	pages, roots := rowsFromLogs(t, logs)
	if len(roots) != 1 {
		t.Fatalf("the chain emitted %d root events, want exactly 1 — the commitment the "+
			"indexer keys on is missing or duplicated", len(roots))
	}
	if roots[0].Root != b.tree.Root() {
		t.Fatalf("the root the contract logged (%s) is not the one submitted (%s)",
			roots[0].Root, b.tree.Root())
	}
	book, err := proofsvc.BuildBook(roots[0], pages)
	if err != nil {
		t.Fatalf("rebuilding the book from the chain's own logs failed: %v\n"+
			"  the indexer cannot serve an epoch the contract published", err)
	}

	// claim with EXACTLY what the service would hand a claimant
	share, path, err := book.Proof("hive:alice")
	if err != nil {
		t.Fatalf("no proof for hive:alice: %v", err)
	}
	before := balanceOf(t, ct, tokenID, "hive:alice")
	_ = before
	call(t, ct, mkDist, "claim", fmt.Sprintf(
		`{"channel":"content","epoch":"0","share":"%s","proof":"%s"}`,
		share, strings.Join(path, ",")), "hive:alice", 2, true)
	after := balanceOf(t, ct, tokenID, "hive:alice")
	b0, _ := new(big.Int).SetString(before, 10)
	b1, _ := new(big.Int).SetString(after, 10)
	if b1.Cmp(b0) <= 0 {
		t.Fatalf("the claim was accepted but paid nothing: %s -> %s", before, after)
	}
}

// The indexer holds the book; the reporter can recompute it from public data with
// no indexer at all. Those are the system's two answers to "what did I earn", and
// a claimant is told the second is a check on the first — so they must be the same
// bytes, not merely both plausible.
func TestIndexerAndReporterAgreeOnTheSameBook(t *testing.T) {
	shares := mkShares(9)
	entries := []string{}
	for _, l := range shareBookBig(shares).tree.Leaves {
		entries = append(entries, l.Account+":"+l.Share)
	}
	joined := strings.Join(entries, ",")

	fromReporter, _ := sharecore.ParseEntries(joined)
	reporterRoot := sharecore.BuildTree(fromReporter).Root()

	// the indexer's path: pages of the same entries, rebuilt and root-checked
	half := (len(entries) + 1) / 2
	pages := []proofsvc.SharesRow{}
	for i, chunk := range [][]string{entries[:half], entries[half:]} {
		sub := strings.Join(chunk, ",")
		s, _ := sharecore.ParseEntries(sub)
		pages = append(pages, proofsvc.SharesRow{
			Channel: "content", Epoch: 0, Page: int64(i), Entries: int64(len(s)),
			PageTotal: proofsvc.Numeric(sharecore.TotalOf(s).String()), Submitted: sub,
		})
	}
	book, err := proofsvc.BuildBook(proofsvc.RootRow{
		Channel: "content", Epoch: 0, Root: reporterRoot,
		TotalShares: proofsvc.Numeric(sharecore.TotalOf(fromReporter).String()),
		Accounts:    int64(len(fromReporter)),
	}, pages)
	if err != nil {
		t.Fatalf("the indexer rejects a book the reporter computed: %v", err)
	}
	for acct := range fromReporter {
		wantShare, wantPath, _ := func() (string, []string, error) {
			p, _ := sharecore.BuildTree(fromReporter).Proof(acct)
			return fromReporter[acct].String(), p, nil
		}()
		gotShare, gotPath, err := book.Proof(acct)
		if err != nil {
			t.Fatalf("%s: %v", acct, err)
		}
		if gotShare != wantShare || strings.Join(gotPath, ",") != strings.Join(wantPath, ",") {
			t.Fatalf("%s: indexer says %s/%v, reporter says %s/%v — a claimant told to "+
				"cross-check one against the other would see a false alarm, or miss a real one",
				acct, gotShare, gotPath, wantShare, wantPath)
		}
	}
}

// shareBookBig is shareBook for a set already in big.Int form.
func shareBookBig(shares map[string]*big.Int) *book {
	total := new(big.Int)
	for _, v := range shares {
		total.Add(total, v)
	}
	return &book{tree: sharecore.BuildTree(shares), total: total}
}
