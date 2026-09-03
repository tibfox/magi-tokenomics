# Halving (decaying emission) schedule — REMOVED, preserved for possible reuse

> **TL;DR** — Decaying emission was **removed from C2 on 2026-07-26** as out of scope;
> emission is flat today. This file is a restore kit, not live documentation: the exact
> deleted code, its config schema and validation, the audit resolutions that governed
> it, its tests, and a step-by-step checklist — all verified by actually replaying the
> restore into a scratch repo, not merely written down.
>
> **Two things have drifted since**, and both matter if you restore it. The archive
> predates the allowance/pool model: it mints per epoch and latches `terminal` on
> exhaustion, whereas C2 now *pulls from an approved pool* and exhaustion only
> **pauses** emission. So restoring a decaying schedule means changing
> `emissionForEpoch` **only** — ignore every `terminal`-latching step in the checklist,
> which no longer describes how C2 behaves.

**Status:** removed from C2 on 2026-07-26 as out of scope. Emission is now flat.
**This file is the only record.** The code was removed on 2026-07-26, the day *before*
this repo's initial commit (`208edd4`, 2026-07-27), so it is not recoverable from git
history either — do not delete this document.

Everything needed to reintroduce the feature is here: the exact code that was
deleted, its config schema and validation rules, the audit resolutions that
governed it, the tests that covered it, and a step-by-step restore checklist.

**These instructions were verified, not just written.** The restore was replayed
into a scratch copy of the repo on 2026-07-26: C2 rebuilt cleanly with tinygo, and
the archived `TestCovEmit_HalvingAcrossEras` below passed against it unmodified
(era 0 = 100000, era 1 = 50000, era 2 = 25000, with the exact-ratio assertions
holding).

---

## 1. What it did

C2 minted a **decaying** amount per epoch instead of a constant one. Time was
divided into *eras*; each era emitted the previous era's amount multiplied by a
configurable rational ratio.

```
era(epoch)   = floor(epoch * epochLen / halvingPeriod)
annual(era)  = baseAnnual * ratioNum^era / ratioDen^era
emission     = annual(era) * epochLen / blocksPerYear
```

The ratio was **configurable, not hardwired to ½**, so a tenant chose the decay shape:

| `ratioNum/ratioDen` | effect                                   |
|---------------------|------------------------------------------|
| `1/2`               | classic halving                          |
| `2/3`               | gentler, ~33% cut per era                |
| `3/4`               | very gentle                              |
| `1/1`               | no decay (flat — the current behaviour)   |

`ratioNum > ratioDen` was rejected at `init`, because it would make emission *grow*
every era — an unbounded inflation foot-gun.

### Worked example

With `baseAnnual=1000000`, `epochLen=1`, `blocksPerYear=10`, `halvingPeriod=2`:

| epoch | era | emission |
|-------|-----|----------|
| 0, 1  | 0   | 100000   |
| 2, 3  | 1   | 50000    |
| 4, 5  | 2   | 25000    |

---

## 2. Config schema (C2 `init`)

Four fields, all removed:

| field           | type   | meaning                                              |
|-----------------|--------|------------------------------------------------------|
| `halvingPeriod` | uint   | era length **in blocks**; must be a multiple of `epochLen` |
| `ratioNum`      | uint   | decay numerator; `0 < ratioNum <= ratioDen`           |
| `ratioDen`      | uint   | decay denominator; `> 0`                              |
| `maxEras`       | uint   | stop after N eras; `0` = unbounded until `maxSupply`  |

Example init fragment as it used to look:

```json
{"epochLen":"28800","baseAnnual":"1000000","halvingPeriod":"31536000",
 "ratioNum":"1","ratioDen":"2","maxEras":"0","blocksPerYear":"10512000"}
```

At 3s/block: 1 day ≈ 28,800 blocks, 1 year ≈ 10,512,000, 3 years ≈ 31,536,000.

---

## 3. The exact code that was removed

### 3a. `init` validation — `c2-emission/contract/main.go`

Sat between the `baseAnnual` check and the `blocksPerYear` check:

```go
	hp := pu(f(payload, "halvingPeriod"))
	if hp == 0 || hp%el != 0 {
		sdk.Abort("halvingPeriod must be >0 and a multiple of epochLen (R12)")
	}
	setU("cfg_halvingPeriod", hp)
	rn, rd := pu(f(payload, "ratioNum")), pu(f(payload, "ratioDen"))
	if rd == 0 || rn > rd {
		sdk.Abort("need 0<ratioNum<=ratioDen")
	}
	setU("cfg_ratioNum", rn)
	setU("cfg_ratioDen", rd)
	setU("cfg_maxEras", pu(f(payload, "maxEras")))
```

### 3b. Era termination inside `distributeEpoch`'s catch-up loop

First statement in the `for ep := next; ep < current && done < maxCatch; ep++` body:

```go
		// Terminate once maxEras is reached — otherwise emission is 0 but pokes
		// continue forever with growing Exp() cost (LOW).
		if me := getU("cfg_maxEras"); me != 0 && (ep*el)/getU("cfg_halvingPeriod") >= me {
			set("terminal", "1")
			break
		}
```

### 3c. The emission curve

```go
func emissionForEpoch(ep uint64) *big.Int {
	el := getU("cfg_epochLen")
	hp := getU("cfg_halvingPeriod")
	era := (ep * el) / hp
	maxEras := getU("cfg_maxEras")
	if maxEras != 0 && era >= maxEras {
		return new(big.Int)
	}
	base := parseBig(getStr("cfg_baseAnnual"))
	rn := new(big.Int).SetUint64(getU("cfg_ratioNum"))
	rd := new(big.Int).SetUint64(getU("cfg_ratioDen"))
	e := new(big.Int).SetUint64(era)
	numPow := new(big.Int).Exp(rn, e, nil)
	denPow := new(big.Int).Exp(rd, e, nil)
	// annual = base * rn^era / rd^era  (single division)
	annual := new(big.Int).Mul(base, numPow)
	annual.Div(annual, denPow)
	// emission = annual * epochLen / blocksPerYear
	em := new(big.Int).Mul(annual, new(big.Int).SetUint64(el))
	em.Div(em, new(big.Int).SetUint64(getU("cfg_blocksPerYear")))
	return em
}
```

Replaced by the current flat version, which keeps the unused `ep` parameter
precisely so this can be restored without touching any caller:

```go
func emissionForEpoch(_ uint64) *big.Int {
	el := getU("cfg_epochLen")
	base := parseBig(getStr("cfg_baseAnnual"))
	em := new(big.Int).Mul(base, new(big.Int).SetUint64(el))
	em.Div(em, new(big.Int).SetUint64(getU("cfg_blocksPerYear")))
	return em
}
```

---

## 4. Design decisions worth keeping (these were audit outcomes, not preferences)

**R13 — one division, never iterative.** Emission was computed as
`base * num^era / den^era` with a *single* final division. The tempting
alternative — halving repeatedly, once per era — compounds integer truncation at
every step and drifts measurably away from the true curve. Restore the single
division if the feature comes back.

**R12 — `halvingPeriod % epochLen == 0`, enforced at `init`.** If an era boundary
lands mid-epoch, that epoch has no single well-defined rate. Rejecting the config
outright is cheaper than defining split-rate semantics.

**`maxEras` had to latch `terminal`.** Once past the last era the curve returns 0,
but without the explicit check in `distributeEpoch` the keeper would keep poking
forever, and each poke would run an `Exp()` whose exponent grows without bound. The
era check therefore had to exist in *both* places — the curve (returning 0) and the
loop (breaking and latching).

**Ratio bounds.** `ratioDen > 0` prevents a division by zero; `ratioNum <= ratioDen`
prevents an emission curve that grows without limit.

---

## 5. Tests that were removed

Restore these into `itest/coverage_c2_test.go`. They use that file's existing
helpers (`cvBoot`, `cvEmission`, `cvPoke`, `cvSupply`, `cvOwed`, `cvField`,
`cvDrain`, `cvSolo`), which all still exist.

```go
// The emission curve must actually halve at each era boundary. With epochLen=1
// and halvingPeriod=2, epochs {0,1} are era 0, {2,3} era 1, {4,5} era 2.
func TestCovEmit_HalvingAcrossEras(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{"halvingPeriod": "2"})

	e0 := cvEmission(t, &ct, "0", 1)
	e1 := cvEmission(t, &ct, "1", 1)
	e2 := cvEmission(t, &ct, "2", 1)
	e3 := cvEmission(t, &ct, "3", 1)
	e4 := cvEmission(t, &ct, "4", 1)

	assert.Equal(t, "100000", e0.String(), "era 0 emission = baseAnnual*epochLen/blocksPerYear")
	assert.Equal(t, e0.String(), e1.String(), "same era must emit the same amount")
	assert.Equal(t, "50000", e2.String(), "era 1 must be half of era 0")
	assert.Equal(t, e2.String(), e3.String())
	assert.Equal(t, "25000", e4.String(), "era 2 must be a quarter of era 0")

	// and the halving must be exact, not merely decreasing
	assert.Equal(t, 0, new(big.Int).Mul(e2, big.NewInt(2)).Cmp(e0), "era1*2 == era0")
	assert.Equal(t, 0, new(big.Int).Mul(e4, big.NewInt(4)).Cmp(e0), "era2*4 == era0")
}

// ratio 1/1 is the "no halving" configuration — emission must stay flat forever.
func TestCovEmit_FlatRatioNeverDecays(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{
		"halvingPeriod": "2", "ratioNum": "1", "ratioDen": "1",
	})

	base := cvEmission(t, &ct, "0", 1)
	assert.Equal(t, "100000", base.String())
	for _, ep := range []string{"1", "2", "3", "10", "100", "1000"} {
		assert.Equal(t, base.String(), cvEmission(t, &ct, ep, 1).String(),
			"ratio 1/1 must never decay (epoch "+ep+")")
	}
}

// maxEras>0 must both zero the emission curve past the last era AND latch the
// `terminal` flag so pokes stop doing work forever.
func TestCovEmit_MaxErasLatchesTerminal(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{"halvingPeriod": "2", "maxEras": "2"})

	// eras 0 and 1 exist, era 2+ is dead
	assert.Equal(t, "100000", cvEmission(t, &ct, "1", 1).String())
	assert.Equal(t, "50000", cvEmission(t, &ct, "3", 1).String())
	assert.Equal(t, "0", cvEmission(t, &ct, "4", 1).String(), "past maxEras emission must be 0")
	assert.Equal(t, "0", cvEmission(t, &ct, "99", 1).String())

	// a far-future poke distributes exactly the 4 live epochs, then latches
	ret := cvPoke(t, &ct, 100)
	assert.Equal(t, "4", cvField(ret, "distributed"), "only epochs 0..3 are live")

	total := cvSupply(t, &ct, 100)
	assert.Equal(t, "300000", total.String(), "2*100000 (era0) + 2*50000 (era1)")
	assert.Equal(t, "0", cvOwed(t, &ct, cvSolo, "4", 100).String(), "no allocation past maxEras")

	// terminal latched: further pokes are no-ops and mint nothing
	r2 := cvPoke(t, &ct, 500)
	assert.Contains(t, r2, `"terminal":true`, "terminal must be latched")
	assert.Equal(t, total.String(), cvSupply(t, &ct, 500).String(), "no mint after terminal")
}
```

Three entries were also deleted from the bad-`init` gauntlet in the same file
(`TestCovEmit_InitValidationRejectsBadConfig`). They need contract ids `cvBad1`, `cvBad5`,
`cvBad6`, which still exist:

```go
		{cvBad1, "halvingPeriod not a multiple of epochLen",
			map[string]string{"epochLen": "3"}},
		{cvBad5, "ratioDen=0", map[string]string{"ratioDen": "0"}},
		{cvBad6, "ratioNum>ratioDen (emission would GROW)",
			map[string]string{"ratioNum": "3", "ratioDen": "2"}},
```

And `TestCovEmit_CatchUpAfterDowntime` asserted era-crossing values, which are now
flat. Its halving-era form was:

```go
	// halvingPeriod=4 with 1-block epochs => the catch-up spans THREE eras
	cvBoot(t, &ct, "1000000000", map[string]string{"halvingPeriod": "4"})
	...
	assert.Equal(t, "100000", want["0"])
	assert.Equal(t, "100000", want["3"])
	assert.Equal(t, "50000", want["4"], "era 1 starts at epoch 4")
	assert.Equal(t, "50000", want["7"])
	assert.Equal(t, "25000", want["8"], "era 2 starts at epoch 8")
	assert.Equal(t, "650000", sum.String())
```

---

## 6. Restore checklist

1. **`c2-emission/contract/main.go`** — put back §3a (init validation), §3b (era
   termination in `distributeEpoch`), and §3c (the curve). The `emissionForEpoch`
   signature already takes the epoch index, so no caller changes.
2. **Rebuild the wasm:**
   ```sh
   cd /home/dockeruser/okinoko/magi-tokenomics
   GOTOOLCHAIN=go1.25.3 tinygo build -gc=custom -scheduler=none -panic=trap \
     -no-debug -target=wasm-unknown -o c2-emission/artifacts/main.wasm ./c2-emission/contract
   ```
3. **Add the four fields back to every C2 `init` payload.** They are required, so
   omitting them aborts. Most of `itest` routes through `cvCfg`'s defaults map in
   `coverage_c2_test.go` — add there:
   ```go
   		"halvingPeriod": "10",
   		"ratioNum":      "1",
   		"ratioDen":      "2",
   		"maxEras":       "0",
   ```
   Tests that build their init JSON inline still need them individually: the rest
   of `itest/*.go`, plus all three devnet tests under
   `magi-tokenomics-spec/reference/vsc-eco/go-vsc-node/tests/devnet/`. Find them
   with `grep -rln '"baseAnnual"' itest/ <devnet dir>`.
4. **Restore the tests in §5**, and replace the current
   `TestCovEmit_EmissionIsFlatForever` /
   `TestCovEmit_SupplyHeadroomIsTheOnlyTerminator` (or keep both — flat is just
   `ratio 1/1`, so they remain valid as a configuration test).
5. **Re-mark audit resolutions R12 and R13 as active** in the spec doc
   (`magi-tokenomics-spec/IMPLEMENTATION_PLAN.md`, §18). Both are currently struck
   through as WITHDRAWN.
6. Run `GOTOOLCHAIN=go1.25.3 go test ./itest/ -count=1 -p 1`.

---

## 7. What replaced it

Emission is flat and **`maxSupply` headroom is the only terminator**: the last
epoch mints whatever remains, `terminal` latches, and subsequent keeper pokes are
permanent no-ops. That behaviour predates this change and is unaffected by it —
under the halving schedule, headroom and `maxEras` were simply two independent
ways to reach the same terminal state.

A consequence worth restating: flat emission does **not** converge. Total issuance
grows linearly and forever, so `maxSupply` is the only thing bounding it. Under the
halving schedule with `ratio < 1`, total emission converged to a finite limit even
with `maxEras = 0`, and `maxSupply` was a backstop rather than the primary bound.
If a tenant needs a hard, time-based issuance cap, that is the argument for
bringing this feature back.
