package vscapi

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// StakeSource resolves a staked balance at a past height from C1's checkpoint
// history, implementing hivesrc.StakeReader.
//
// It mirrors C1's on-chain layout exactly:
//
//	hist_n|<acct>      -> number of history entries for the account
//	hist|<acct>|<i>    -> "<height>:<amount>", heights non-decreasing
//	ckpt_n / ckpt|<i>  -> the same shape for the global total
//
// and C1's search rule: the answer is the RIGHTMOST entry with height <= target
// (0 if none). Any divergence here silently changes payouts, so this must stay a
// faithful copy of c1-staking/contract/main.go's searchVal — including its
// behaviour on a missing entry (stop the walk, do not treat it as height 0).
type StakeSource struct {
	// Client is an interface, not *Client, so the search can be exercised against
	// a synthetic history in tests — this code MUST stay bug-for-bug identical to
	// the contract, which is only provable by testing it.
	Client     StateReader
	ContractID string

	// caches: an epoch's votes come from a small active set, so repeat voters are
	// the common case and each lookup otherwise costs log2(n) round trips.
	cache map[string]*big.Int
	nlen  map[string]uint64
}

// StateReader is the single node capability StakeSource needs.
type StateReader interface {
	StateGetOne(contractID, key string) (string, bool, error)
}

func NewStakeSource(c StateReader, contractID string) *StakeSource {
	return &StakeSource{Client: c, ContractID: contractID,
		cache: map[string]*big.Int{}, nlen: map[string]uint64{}}
}

// StakeAtHeight returns the account's staked balance as of height h.
func (s *StakeSource) StakeAtHeight(account string, h uint64) (*big.Int, error) {
	ck := account + "@" + strconv.FormatUint(h, 10)
	if v, ok := s.cache[ck]; ok {
		return v, nil
	}
	n, err := s.count("hist_n|" + account)
	if err != nil {
		return nil, err
	}
	v, err := s.search(func(i uint64) string {
		return "hist|" + account + "|" + strconv.FormatUint(i, 10)
	}, n, h)
	if err != nil {
		return nil, err
	}
	s.cache[ck] = v
	return v, nil
}

// TotalAtHeight returns the global staked total as of height h. The reporter does
// not need it for share math (C3 works on raw shares, not fractions) but it is the
// cheapest sanity check that the snapshot height is one C1 actually has data for.
func (s *StakeSource) TotalAtHeight(h uint64) (*big.Int, error) {
	n, err := s.count("ckpt_n")
	if err != nil {
		return nil, err
	}
	return s.search(func(i uint64) string {
		return "ckpt|" + strconv.FormatUint(i, 10)
	}, n, h)
}

func (s *StakeSource) count(key string) (uint64, error) {
	if v, ok := s.nlen[key]; ok {
		return v, nil
	}
	raw, ok, err := s.Client.StateGetOne(s.ContractID, key)
	if err != nil {
		return 0, err
	}
	var n uint64
	if ok {
		n, err = strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vsc api: %s is not a uint: %q", key, raw)
		}
	}
	s.nlen[key] = n
	return n, nil
}

// search is the off-chain twin of C1's searchVal.
func (s *StakeSource) search(keyAt func(uint64) string, n, target uint64) (*big.Int, error) {
	res := new(big.Int)
	if n == 0 {
		return res, nil
	}
	lo, hi := uint64(0), n-1
	found := false
	var foundI uint64
	for lo <= hi {
		mid := (lo + hi) / 2
		raw, ok, err := s.Client.StateGetOne(s.ContractID, keyAt(mid))
		if err != nil {
			return nil, err
		}
		if !ok {
			break // hole in the history: stop, exactly as the contract does
		}
		hs, _ := splitColon(raw)
		hval, err := strconv.ParseUint(hs, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("vsc api: malformed history entry %q", raw)
		}
		if hval <= target {
			found = true
			foundI = mid
			if mid == n-1 {
				break
			}
			lo = mid + 1
		} else {
			if mid == 0 {
				break
			}
			hi = mid - 1
		}
	}
	if !found {
		return res, nil
	}
	raw, ok, err := s.Client.StateGetOne(s.ContractID, keyAt(foundI))
	if err != nil {
		return nil, err
	}
	if !ok {
		return res, nil
	}
	_, amt := splitColon(raw)
	v, good := new(big.Int).SetString(amt, 10)
	if !good {
		return nil, fmt.Errorf("vsc api: malformed amount in %q", raw)
	}
	return v, nil
}

func splitColon(s string) (string, string) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}
