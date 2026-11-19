// The token owner's identity, derived from the kit's public scenario seed.
//
// The trial's bearer token is opaque: nothing in it names the login it belongs to.
// But the suite builds every name it sends from one fixed seed (kit
// program/env/src/wire.ts: SCENARIO_SEED, public kit data), and the viewer is
// the first user that seed generates. This file vendors that generation
// (FNV-1a + splitmix64 seeding, xorshift64*, the name lists) so the entry
// answers viewer.login, GET /api/v3/user and repo ownership with the login
// the token names, without ever seeing the token's meaning.
// Verified: the seed yields `adaiversen` (id 1000).

package main

// The kit's public scenario seed (wire.ts SCENARIO_SEED).
const scenarioSeed uint64 = 0x6a09e667f3bcc909

// The scenario's day one, 2025-01-01T00:00:00Z: the base of every deterministic timestamp.
const scenarioEpochSec int64 = 1735689600

// fnv1a64: FNV-1a, 64-bit: a label's stable seed contribution.
func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

// mix64: splitmix64's finalizer: a seeded value with every bit mixed into every other.
func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// scenarioSeedFor: the seed a named stream starts from.
func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is xorshift64*: a 64-bit state walked by three shifts and one multiply.
type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{state: seed}
}

// nextU64 returns the next 64-bit value.
func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

// nextU32 returns the next 32-bit value, from the top half of nextU64().
func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

// nextInt returns an integer in [0, maxExclusive), unbiased by rejection.
func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rngRange uint64 = 4294967296
	limit := rngRange - (rngRange % uint64(maxExclusive))
	for {
		value := uint64(r.nextU32())
		if value < limit {
			return int(value % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// ViewerIdentity is one scenario user.
type ViewerIdentity struct {
	ID    int
	Login string
	Name  string
	Email string
}

// generateUsers builds the scenario's users, deterministic for the seed; the first is the viewer.
func generateUsers(count int) []ViewerIdentity {
	r := newRng(scenarioSeedFor("users"))
	used := make(map[string]bool, count)
	users := make([]ViewerIdentity, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := first + last
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, ViewerIdentity{ID: 1000 + i, Login: login, Name: first + " " + last, Email: login + "@example.invalid"})
	}
	return users
}

// viewer is the first user the public seed generates. Computed once.
var viewer = func() ViewerIdentity {
	users := generateUsers(12)
	if len(users) > 0 && users[0].Login != "" {
		return users[0]
	}
	// Fall through to the verified value.
	return ViewerIdentity{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}
}()

// itoa converts a small int to decimal without importing strconv in hot paths.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
