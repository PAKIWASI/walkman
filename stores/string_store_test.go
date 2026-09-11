package stores

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

func TestStoreAndRetrieveBasic(t *testing.T) {
	ss := NewStringStore()
	s1 := ss.StoreString("hello")
	s2 := ss.StoreString("world")
	if s1 != "hello" {
		t.Fatalf("s1 = %q, want %q", s1, "hello")
	}
	if s2 != "world" {
		t.Fatalf("s2 = %q, want %q", s2, "world")
	}
	// s1 must still read back correctly after s2 was written right after it
	if s1 != "hello" {
		t.Fatalf("s1 corrupted after later store: %q", s1)
	}
}

func TestStoreBytesRoundTrip(t *testing.T) {
	ss := NewStringStore()
	b := []byte{0x00, 0xFF, 'a', 'b', 0x00}
	got := ss.StoreBytes(b)
	if got != string(b) {
		t.Fatalf("got %q, want %q", got, string(b))
	}
}

func TestEmptyString(t *testing.T) {
	ss := NewStringStore()
	got := ss.StoreString("")
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestExactNodeBoundary stores a string that exactly fills the
// remaining space in a node and checks it doesn't panic or corrupt.
func TestExactNodeBoundaryStr(t *testing.T) {
	ss := NewStringStore()
	prefix := ss.StoreString("abc") // off is now 3
	exact := strings.Repeat("x", stringStoreNodeSize-3)
	got := ss.StoreString(exact)
	if got != exact {
		t.Fatalf("exact-fit string corrupted: len(got)=%d want %d", len(got), len(exact))
	}
	if prefix != "abc" {
		t.Fatalf("prefix corrupted: %q", prefix)
	}
	// one more byte must force growth into a new node without panicking
	got2 := ss.StoreString("y")
	if got2 != "y" {
		t.Fatalf("post-growth store corrupted: %q", got2)
	}
}

// TestGrowsAcrossManyNodes forces several node growths sequentially.
func TestGrowsAcrossManyNodesStr(t *testing.T) {
	ss := NewStringStore()
	var want []string
	for i := 0; i < 50; i++ {
		s := fmt.Sprintf("chunk-%d-%s", i, strings.Repeat("z", 200))
		got := ss.StoreString(s)
		if got != s {
			t.Fatalf("chunk %d corrupted: got %q want %q", i, got, s)
		}
		want = append(want, got)
	}
	// re-check all earlier ones are still intact after many more writes
	for i, s := range want {
		expected := fmt.Sprintf("chunk-%d-%s", i, strings.Repeat("z", 200))
		if s != expected {
			t.Fatalf("chunk %d corrupted after later writes: got %q want %q", i, s, expected)
		}
	}
}

// TestOversizedString exercises the heap-fallback path for strings
// bigger than a single node.
func TestOversizedString(t *testing.T) {
	ss := NewStringStore()
	big := strings.Repeat("q", stringStoreNodeSize+1)
	got := ss.StoreString(big)
	if got != big {
		t.Fatalf("oversized string corrupted: len(got)=%d want %d", len(got), len(big))
	}

	// mix an oversized store with normal ones around it
	small1 := ss.StoreString("before")
	huge := ss.StoreString(strings.Repeat("Q", stringStoreNodeSize*3))
	small2 := ss.StoreString("after")
	if small1 != "before" || small2 != "after" || huge != strings.Repeat("Q", stringStoreNodeSize*3) {
		t.Fatalf("oversized store corrupted neighboring stores: before=%q after=%q hugeLen=%d",
			small1, small2, len(huge))
	}
}

// TestConcurrentNoCorruption hammers the store from many goroutines
// with unique content and checks every returned string matches exactly
// what was stored, with no cross-goroutine corruption or overlap.
// Run with -race.
func TestConcurrentNoCorruption(t *testing.T) {
	ss := NewStringStore()
	const goroutines = 64
	const perGoroutine = 500

	var wg sync.WaitGroup
	errs := make(chan string, goroutines*perGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < perGoroutine; i++ {
				n := r.Intn(300) + 1 // vary sizes, including some near/over boundary occasionally
				want := fmt.Sprintf("g%d-i%d-%s", g, i, strings.Repeat(string(rune('A'+g%26)), n))
				got := ss.StoreString(want)
				if got != want {
					errs <- fmt.Sprintf("g=%d i=%d: got %q want %q", g, i, got, want)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestConcurrentMixedWithOversized mixes normal-size and oversized
// (heap-fallback) stores concurrently.
func TestConcurrentMixedWithOversized(t *testing.T) {
	ss := NewStringStore()
	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var want string
			if g%5 == 0 {
				want = strings.Repeat(fmt.Sprintf("%d", g%10), stringStoreNodeSize+100)
			} else {
				want = fmt.Sprintf("small-%d", g)
			}
			got := ss.StoreString(want)
			if got != want {
				errs <- fmt.Sprintf("g=%d: got len %d want len %d (mismatch)", g, len(got), len(want))
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestZeroLengthUnderContention makes sure empty-string stores (a
// zero-byte claim) don't misbehave under heavy concurrent traffic.
func TestZeroLengthUnderContention(t *testing.T) {
	ss := NewStringStore()
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if got := ss.StoreString(""); got != "" {
					t.Errorf("got %q, want empty", got)
				}
			}
		}()
	}
	wg.Wait()
}
