package sharecore

import (
	"math/big"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// A merkle share book: the chain holds a 32-byte commitment instead of one state
// entry per earner, and a claimant supplies the proof.
//
// WHY. Writing a share book costs ~311 RC per account and a log costs ~1 RC per
// byte, so at 500 earners the state writes are ~92% of an epoch's bill. Replacing
// them with a root takes an epoch from ~127,500 RC to ~15,000, and the part that
// remains is the leaf list itself — logged, not stored, so the indexer still holds
// the whole share book and anyone can rebuild the tree from it.
//
// The trade, stated plainly: `share|<ch>|<ep>|<acct>` no longer exists on chain. A
// wallet cannot read what an account earned from chain state alone; it asks the
// indexer, and the root is what proves the answer was not invented.
//
// CONSTRUCTION, and every detail here is load-bearing:
//
//	leaf     = sha256( 0x00 || "acct|share" )
//	internal = sha256( 0x01 || min(a,b) || max(a,b) )
//
//   - The 0x00/0x01 domain tags stop a leaf being reinterpreted as an internal node.
//     Without them an attacker who can choose an account name could present an
//     internal node's preimage as a leaf and prove membership of something that was
//     never in the tree (the classic second-preimage attack on Merkle trees).
//   - The pair is SORTED before hashing, so a proof needs no direction bits — the
//     payload is just a list of siblings. This proves set membership rather than
//     position, which is exactly what a claim needs.
//   - An odd node at any level is PROMOTED unchanged, never duplicated. Duplicating
//     it makes two different leaf sets produce the same root (CVE-2012-2459).
//   - Leaves are sorted by account, which is already the canonical order, so the tree
//     shape is a function of the share set alone and two reporters agree by
//     construction.
//
// The leaf binds account AND share together, so a proof cannot be replayed under a
// different name or with a larger number: change either and the leaf hash changes.

// Leaf is one earner's entry in the tree.
type Leaf struct {
	Account string
	Share   string // decimal, exactly as it appears in the canonical list
}

// LeafHash returns the tagged hash of one leaf.
func LeafHash(account, share string) [32]byte {
	b := make([]byte, 0, 1+len(account)+1+len(share))
	b = append(b, 0x00)
	b = append(b, account...)
	b = append(b, '|')
	b = append(b, share...)
	return sha256.Sum256(b)
}

// nodeHash combines two children, sorted so a proof carries no direction.
func nodeHash(a, b [32]byte) [32]byte {
	buf := make([]byte, 0, 1+64)
	buf = append(buf, 0x01)
	if lessHash(a, b) {
		buf = append(buf, a[:]...)
		buf = append(buf, b[:]...)
	} else {
		buf = append(buf, b[:]...)
		buf = append(buf, a[:]...)
	}
	return sha256.Sum256(buf)
}

func lessHash(a, b [32]byte) bool {
	for i := 0; i < 32; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Tree is a built merkle tree over one epoch's shares.
type Tree struct {
	Leaves []Leaf     // sorted by account
	levels [][][32]byte // levels[0] = leaf hashes, last = the root
}

// BuildTree constructs the tree from a share set. The map is sorted by account, so
// the result depends only on the shares themselves.
func BuildTree(shares map[string]*big.Int) *Tree {
	accts := make([]string, 0, len(shares))
	for a := range shares {
		accts = append(accts, a)
	}
	sort.Strings(accts)

	t := &Tree{Leaves: make([]Leaf, 0, len(accts))}
	level := make([][32]byte, 0, len(accts))
	for _, a := range accts {
		s := shares[a].String()
		t.Leaves = append(t.Leaves, Leaf{Account: a, Share: s})
		level = append(level, LeafHash(a, s))
	}
	if len(level) == 0 {
		return t
	}
	t.levels = append(t.levels, level)
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i]) // promote, never duplicate
				continue
			}
			next = append(next, nodeHash(level[i], level[i+1]))
		}
		t.levels = append(t.levels, next)
		level = next
	}
	return t
}

// Root is the commitment submitted on chain, hex-encoded. Empty for an empty tree.
func (t *Tree) Root() string {
	if len(t.levels) == 0 {
		return ""
	}
	top := t.levels[len(t.levels)-1]
	return hex.EncodeToString(top[0][:])
}

// Proof returns the sibling path for an account, hex-encoded, or false if the
// account is not in the tree.
func (t *Tree) Proof(account string) ([]string, bool) {
	idx := -1
	for i, l := range t.Leaves {
		if l.Account == account {
			idx = i
			break
		}
	}
	if idx < 0 || len(t.levels) == 0 {
		return nil, false
	}
	var out []string
	for lvl := 0; lvl < len(t.levels)-1; lvl++ {
		nodes := t.levels[lvl]
		sib := idx ^ 1
		// a promoted odd node has no sibling at this level
		if sib < len(nodes) {
			out = append(out, hex.EncodeToString(nodes[sib][:]))
		}
		idx /= 2
	}
	return out, true
}

// VerifyProof recomputes the root from a leaf and its siblings. This is the exact
// algorithm the contract implements; keeping a reference copy here lets the reporter
// check its own output before anything is submitted.
func VerifyProof(account, share string, proof []string, root string) bool {
	h := LeafHash(account, share)
	for _, p := range proof {
		raw, err := hex.DecodeString(p)
		if err != nil || len(raw) != 32 {
			return false
		}
		var sib [32]byte
		copy(sib[:], raw)
		h = nodeHash(h, sib)
	}
	return hex.EncodeToString(h[:]) == root
}
