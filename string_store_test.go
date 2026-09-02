package walkman

import (
	"os"
	"testing"
)

func TestStringStore_StorePath(t *testing.T) {
	sep := string(os.PathSeparator)
	tests := []struct {
		name   string
		parent string
		child  string
		want   string
	}{
		{"without_trailing_slash", "parent", "child", "parent" + sep + "child"},
		{"with_trailing_slash", "parent" + sep, "child", "parent" + sep + "child"},
		{"dot_parent", ".", "child", "." + sep + "child"},
	}

	p := newStringStore(0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.storePath(tt.parent, tt.child)
			if str := p.retrieve(got); str != tt.want {
				t.Errorf("storePath(%q, %q) = %q, want %q", tt.parent, tt.child, str, tt.want)
			}
		})
	}
}

func TestStringStore_StoreAndRetrieve(t *testing.T) {
	p := newStringStore(0)

	id1 := p.store("hello")
	id2 := p.store("world")
	id3 := p.store("")

	if got := p.retrieve(id1); got != "hello" {
		t.Errorf("retrieve(id1) = %q, want %q", got, "hello")
	}
	if got := p.retrieve(id2); got != "world" {
		t.Errorf("retrieve(id2) = %q, want %q", got, "world")
	}
	if got := p.retrieve(id3); got != "" {
		t.Errorf("retrieve(id3) = %q, want %q", got, "")
	}
}

func TestStringStore_GrowthUnderHighVolume(t *testing.T) {
	p := newStringStore(64) // tiny initial capacity to force many resizes
	const count = 5000

	type stored struct {
		id   stringID
		want string
	}

	var items []stored
	for i := range count {
		parent := "root/dir" + itoa(i)
		child := "leaf" + itoa(i) + ".txt"
		want := parent + string(os.PathSeparator) + child
		id := p.storePath(parent, child)
		items = append(items, stored{id: id, want: want})
	}

	// Verify all items are still intact and uncorrupted after numerous buffer reallocations
	for i, it := range items {
		if got := p.retrieve(it.id); got != it.want {
			t.Fatalf("item %d: retrieve = %q, want %q", i, got, it.want)
		}
	}
}

func TestStringStore_DeepNesting(t *testing.T) {
	p := newStringStore(0)
	current := "root"

	for level := 1; level <= 60; level++ {
		child := "level" + itoa(level)
		want := current + string(os.PathSeparator) + child
		id := p.storePath(current, child)
		if got := p.retrieve(id); got != want {
			t.Fatalf("level %d: retrieve = %q, want %q", level, got, want)
		}
		current = want
	}
}



