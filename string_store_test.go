package walkman_test

import(
	"github.com/PAKIWASI/walkman"
	"testing"
)

func TestStringStore_StorePath(t *testing.T) {
	tests := []struct {
		name string
		parent string
		child  string
		want   string
	}{
		{"test", "parent", "child", "parent/child"},
		{"test2", "parent/", "child", "parent/child"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := walkman.NewPathStorage(0)
			got := p.StorePath(tt.parent, tt.child)
			if got.String() != tt.want {
				t.Errorf("StorePath() = %v, want %v", got, tt.want)
			}
		})
	}
}

