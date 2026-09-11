package stores

import (
	"fmt"
	"sync"
	"testing"
)

type Point struct {
	X, Y int
}

type WithSlice struct {
	ID   int
	Data []int
}

func TestAppendBasicTypes(t *testing.T) {
	ints := NewStore[int]()
	p := ints.Append(42)
	if *p != 42 {
		t.Fatalf("*p = %d, want 42", *p)
	}

	floats := NewStore[float64]()
	pf := floats.Append(3.14)
	if *pf != 3.14 {
		t.Fatalf("*pf = %v, want 3.14", *pf)
	}

	bools := NewStore[bool]()
	pb := bools.Append(true)
	if *pb != true {
		t.Fatalf("*pb = %v, want true", *pb)
	}
}

func TestAppendSimpleStruct(t *testing.T) {
	points := NewStore[Point]()
	p1 := points.Append(Point{1, 2})
	p2 := points.Append(Point{3, 4})
	if *p1 != (Point{1, 2}) {
		t.Fatalf("p1 = %v, want {1 2}", *p1)
	}
	if *p2 != (Point{3, 4}) {
		t.Fatalf("p2 = %v, want {3 4}", *p2)
	}
	if p1 == p2 {
		t.Fatalf("p1 and p2 point to the same address: %p", p1)
	}
}

func TestAppendSliceRoundTrip(t *testing.T) {
	points := NewStore[Point]()
	in := []Point{{1, 2}, {3, 4}, {5, 6}}
	got := points.AppendSlice(in)
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("got[%d] = %v, want %v", i, got[i], in[i])
		}
	}
	// mutating the input after the fact must not affect the stored copy
	in[0] = Point{999, 999}
	if got[0] == (Point{999, 999}) {
		t.Fatalf("stored copy aliased the input slice")
	}
}

func TestAppendSliceEmpty(t *testing.T) {
	s := NewStore[int]()
	got := s.AppendSlice(nil)
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

// TestExactNodeBoundary fills a node exactly and confirms the next
// append forces growth without panicking.
func TestExactNodeBoundary(t *testing.T) {
	const nodeSize = 8
	s := NewStore[int]()
	for i := 0; i < nodeSize; i++ {
		p := s.Append(i)
		if *p != i {
			t.Fatalf("elem %d = %d, want %d", i, *p, i)
		}
	}
	// one more must grow to a new node
	p := s.Append(999)
	if *p != 999 {
		t.Fatalf("post-growth elem = %d, want 999", *p)
	}
}

// TestGrowsAcrossManyNodes forces many sequential node growths with a
// tiny node size and checks nothing gets corrupted along the way.
func TestGrowsAcrossManyNodes(t *testing.T) {
	const nodeSize = 3
	s := NewStore[int]()
	var ptrs []*int
	for i := 0; i < 500; i++ {
		ptrs = append(ptrs, s.Append(i))
	}
	for i, p := range ptrs {
		if *p != i {
			t.Fatalf("elem %d corrupted: got %d want %d", i, *p, i)
		}
	}
}

// TestOversizedBatch exercises the heap-fallback path for a batch
// bigger than a single node.
func TestOversizedBatch(t *testing.T) {
	const nodeSize = 4
	s := NewStore[int]()
	big := make([]int, nodeSize*10)
	for i := range big {
		big[i] = i
	}
	got := s.AppendSlice(big)
	for i := range big {
		if got[i] != big[i] {
			t.Fatalf("got[%d] = %d, want %d", i, got[i], big[i])
		}
	}
	// mix with normal small appends around it
	before := s.Append(-1)
	huge := s.AppendSlice(big)
	after := s.Append(-2)
	if *before != -1 || *after != -2 {
		t.Fatalf("oversized batch corrupted neighbors: before=%d after=%d", *before, *after)
	}
	for i := range big {
		if huge[i] != big[i] {
			t.Fatalf("huge[%d] = %d, want %d", i, huge[i], big[i])
		}
	}
}

// TestStructWithSliceField documents/verifies the shallow-copy
// behavior: the slice header is copied into the arena, but the
// backing array is shared with the original.
func TestStructWithSliceField(t *testing.T) {
	s := NewStore[WithSlice]()
	data := []int{1, 2, 3}
	p := s.Append(WithSlice{ID: 1, Data: data})
	data[0] = 999 // mutate shared backing array
	if p.Data[0] != 999 {
		t.Fatalf("expected shared backing array to reflect mutation (documenting shallow-copy semantics), got %v", p.Data)
	}
}

// TestConcurrentAppendNoCorruption hammers Append from many goroutines
// with a small node size (heavy growth + contention) and checks every
// returned pointer holds exactly what was written, with unique
// addresses. Run with -race.
func TestConcurrentAppendNoCorruption(t *testing.T) {
	const nodeSize = 8
	s := NewStore[int]()
	const goroutines = 64
	const perGoroutine = 500

	var wg sync.WaitGroup
	type result struct {
		ptr  *int
		want int
	}
	results := make(chan result, goroutines*perGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				want := g*perGoroutine + i
				p := s.Append(want)
				results <- result{p, want}
			}
		}(g)
	}
	wg.Wait()
	close(results)

	seen := make(map[*int]bool)
	for r := range results {
		if *r.ptr != r.want {
			t.Errorf("ptr %p = %d, want %d", r.ptr, *r.ptr, r.want)
		}
		if seen[r.ptr] {
			t.Errorf("duplicate pointer %p returned to two different appends", r.ptr)
		}
		seen[r.ptr] = true
	}
}

// TestConcurrentAppendSlice hammers AppendSlice from many goroutines
// with varying batch sizes, some exceeding the node size, and checks
// every returned batch is internally consistent. Run with -race.
func TestConcurrentAppendSlice(t *testing.T) {
	const nodeSize = 16
	s := NewStore[int]()
	const goroutines = 48

	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			n := (g % 30) + 1 // sometimes exceeds nodeSize
			batch := make([]int, n)
			for i := range batch {
				batch[i] = g*1000 + i
			}
			got := s.AppendSlice(batch)
			for i := range batch {
				if got[i] != batch[i] {
					errs <- fmt.Sprintf("g=%d elem %d: got %d want %d", g, i, got[i], batch[i])
					return
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

// TestConcurrentStructAppend does the same contention stress test but
// with a multi-field struct, to check for partial/torn writes across
// fields under a small node size.
func TestConcurrentStructAppend(t *testing.T) {
	const nodeSize = 5
	s := NewStore[Point]()
	const goroutines = 64
	const perGoroutine = 300

	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				want := Point{X: g, Y: i}
				p := s.Append(want)
				if *p != want {
					errs <- fmt.Sprintf("g=%d i=%d: got %v want %v", g, i, *p, want)
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
