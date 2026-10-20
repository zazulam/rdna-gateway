package prefix

import (
	"strings"
	"testing"
)

func TestChainSharesPrefixAcrossTurns(t *testing.T) {
	ix := New(8, 100)
	a := ix.Chain("aaaaaaaabbbbbbbbcccc")             // 2 full blocks
	b := ix.Chain("aaaaaaaabbbbbbbbccccccccdddddddd") // 4 full blocks
	if len(a) != 2 || len(b) != 4 {
		t.Fatalf("lens %d %d", len(a), len(b))
	}
	if a[0] != b[0] || a[1] != b[1] {
		t.Fatal("shared prefix should hash identically")
	}
	// Same block content at a different position must not collide.
	c := ix.Chain("bbbbbbbbaaaaaaaa")
	if c[1] == a[0] {
		t.Fatal("chained hash must depend on what came before")
	}
}

func TestMatchIsContiguousPerWorker(t *testing.T) {
	ix := New(4, 100)
	long := ix.Chain("0000111122223333")
	ix.Insert(long, "a")
	ix.Insert(long[:2], "b")
	m := ix.Match(long)
	if m["a"] != 4 || m["b"] != 2 {
		t.Fatalf("got %v", m)
	}
	// A diverging prompt matches only the shared part.
	other := ix.Chain("00001111zzzzyyyy")
	m = ix.Match(other)
	if m["a"] != 2 || m["b"] != 2 {
		t.Fatalf("diverging: %v", m)
	}
}

func TestEvictionIsLRUAndBounded(t *testing.T) {
	ix := New(4, 3)
	ix.Insert(ix.Chain("aaaabbbb"), "w") // 2 nodes
	ix.Insert(ix.Chain("ccccdddd"), "w") // 2 more → evicts oldest
	if ix.Len() != 3 {
		t.Fatalf("len %d", ix.Len())
	}
	if got := ix.Match(ix.Chain("aaaabbbb"))["w"]; got != 0 {
		t.Fatalf("oldest root should be evicted, matched %d", got)
	}
	if got := ix.Match(ix.Chain("ccccdddd"))["w"]; got != 2 {
		t.Fatalf("recent prefix should survive, matched %d", got)
	}
}

func TestForgetWorker(t *testing.T) {
	ix := New(4, 100)
	ch := ix.Chain(strings.Repeat("x", 16))
	ix.Insert(ch, "a")
	ix.Insert(ch, "b")
	ix.ForgetWorker("a")
	m := ix.Match(ch)
	if _, ok := m["a"]; ok || m["b"] != 4 {
		t.Fatalf("got %v", m)
	}
	ix.ForgetWorker("b")
	if ix.Len() != 0 {
		t.Fatalf("empty nodes should be dropped, len %d", ix.Len())
	}
}
