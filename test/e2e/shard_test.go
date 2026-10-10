//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"testing"
)

// fakeTB records the Fatalf/Skipf a harness gate issues instead of ending the
// test, so the gate's decision is observable. Any other testing.TB method
// panics on the nil embedded interface.
type fakeTB struct {
	testing.TB

	name  string
	fatal string
	skip  string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Name() string { return f.name }

func (f *fakeTB) Fatalf(format string, args ...any) { f.fatal = fmt.Sprintf(format, args...) }

func (f *fakeTB) Skipf(format string, args ...any) { f.skip = fmt.Sprintf(format, args...) }

// nameOnOtherShard returns the first prefix+N whose shard is not index.
func nameOnOtherShard(prefix string, index, total int) string {
	for n := 0; ; n++ {
		if name := prefix + strconv.Itoa(n); shardOf(name, total) != index {
			return name
		}
	}
}

// TestShardSkipSubtestsFollowClaim checks a subtest runs on the shard its
// ancestor claimed even when its own name hashes elsewhere, and that a test
// with no claimed ancestor still hashes on its own name.
func TestShardSkipSubtestsFollowClaim(t *testing.T) {
	const total = 4

	parent := t.Name() + "/fixture"
	index := shardOf(parent, total)

	t.Setenv("E2E_SHARD_TOTAL", strconv.Itoa(total))
	t.Setenv("E2E_SHARD_INDEX", strconv.Itoa(index))

	leg := nameOnOtherShard(parent+"/leg", index, total)

	early := &fakeTB{name: leg}
	shardSkip(early)

	if early.skip == "" {
		t.Errorf("%s ran before its ancestor claimed shard %d; want it hashed on its own name and skipped", leg, index)
	}

	shardSkip(&fakeTB{name: parent})

	claimed := &fakeTB{name: leg}
	shardSkip(claimed)

	if claimed.skip != "" || claimed.fatal != "" {
		t.Errorf("%s under claimed %s: skip=%q fatal=%q, want it run", leg, parent, claimed.skip, claimed.fatal)
	}

	unrelated := &fakeTB{name: nameOnOtherShard(t.Name()+"/other", index, total)}
	shardSkip(unrelated)

	if unrelated.skip == "" {
		t.Errorf("%s hashes off shard %d with no claimed ancestor; want it skipped", unrelated.name, index)
	}
}
