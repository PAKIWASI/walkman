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


