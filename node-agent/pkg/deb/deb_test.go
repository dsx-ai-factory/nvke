package deb

import (
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallRemove(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping test on non-Linux platform")
	}
	if os.Geteuid() != 0 {
		t.Skip("skipping apt integration test: requires root")
	}

	ctx := t.Context()

	t.Log("updating database")
	err := Update(ctx)
	if err != nil {
		// Root in a restricted sandbox (e.g. no apt lock / lists write) cannot run this.
		t.Skipf("skipping apt integration test: apt update unavailable: %v", err)
	}

	t.Log("installing packages")
	err = Install(ctx, "ca-certificates", "cowsay")
	require.NoError(t, err)

	t.Log("listing packages")
	iter, err := List(ctx)
	assert.NoError(t, err)

	index := slices.IndexFunc(slices.Collect(iter), func(p Package) bool {
		return p.Name == "cowsay"
	})
	require.NotEqualValues(t, -1, index)

	t.Log("removing packages")
	err = Remove(ctx, true, "cowsay")
	assert.NoError(t, err)

	iter, err = List(ctx)
	assert.NoError(t, err)

	index = slices.IndexFunc(slices.Collect(iter), func(p Package) bool {
		return p.Name == "cowsay"
	})
	assert.EqualValues(t, -1, index)
}
