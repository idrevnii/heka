package history

import (
	"sync"
	"testing"
	"time"
)

func TestWraparound(t *testing.T) {
	s := New(3, 3, 1<<20)
	var ids []uint64
	for i := 0; i < 5; i++ {
		ids = append(ids, s.Add(Record{Route: "r"}))
	}
	list := s.List(Filter{})
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Newest first: ids[4], ids[3], ids[2].
	want := []uint64{ids[4], ids[3], ids[2]}
	for i, r := range list {
		if r.ID != want[i] {
			t.Fatalf("list[%d].ID = %d, want %d", i, r.ID, want[i])
		}
	}
	if _, ok := s.Get(ids[0]); ok {
		t.Fatal("evicted record should not be found")
	}
	if _, ok := s.Get(ids[4]); !ok {
		t.Fatal("most recent record should be found")
	}
}

func TestByteBudgetEviction(t *testing.T) {
	s := New(10, 10, 10) // 10-byte body budget
	id1 := s.Add(Record{Route: "r", ReqBody: []byte("12345")})
	id2 := s.Add(Record{Route: "r", ReqBody: []byte("67890")})
	// Both fit exactly (10 bytes total).
	r1, _ := s.Get(id1)
	if r1.BodyEvicted {
		t.Fatal("id1 should not be evicted yet")
	}
	id3 := s.Add(Record{Route: "r", ReqBody: []byte("abcde")})
	// Now 15 bytes logically requested; oldest (id1) must be evicted first.
	r1, _ = s.Get(id1)
	if !r1.BodyEvicted || r1.ReqBody != nil {
		t.Fatalf("id1 should have been evicted: evicted=%v body=%q", r1.BodyEvicted, r1.ReqBody)
	}
	r2, _ := s.Get(id2)
	if r2.BodyEvicted {
		t.Fatal("id2 should still have its body")
	}
	r3, _ := s.Get(id3)
	if r3.BodyEvicted {
		t.Fatal("id3 (just added) should still have its body")
	}
}

func TestListOmitsBodies(t *testing.T) {
	s := New(10, 10, 1<<20)
	s.Add(Record{Route: "r", ReqBody: []byte("secret")})
	list := s.List(Filter{})
	if len(list) != 1 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].ReqBody != nil {
		t.Fatal("List must not include bodies")
	}
}

func TestFilter(t *testing.T) {
	s := New(10, 10, 1<<20)
	s.Add(Record{Route: "a", Status: 200})
	s.Add(Record{Route: "b", Status: 500})
	s.Add(Record{Route: "a", Status: 429})

	if got := s.List(Filter{Route: "a"}); len(got) != 2 {
		t.Fatalf("route filter = %d, want 2", len(got))
	}
	if got := s.List(Filter{OnlyErrors: true}); len(got) != 2 {
		t.Fatalf("errors filter = %d, want 2", len(got))
	}
	if got := s.List(Filter{MinStatus: 500}); len(got) != 1 {
		t.Fatalf("min_status filter = %d, want 1", len(got))
	}
}

func TestNilStoreIsNoOp(t *testing.T) {
	var s *Store
	if id := s.Add(Record{}); id != 0 {
		t.Fatalf("Add on nil store returned %d", id)
	}
	if list := s.List(Filter{}); list != nil {
		t.Fatal("List on nil store should be nil")
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("Get on nil store should miss")
	}
	s.AddEvent(Event{}) // must not panic
	if errs := s.Errors(10); errs != nil {
		t.Fatal("Errors on nil store should be nil")
	}
	_ = s.Overview(time.Minute)
	_ = s.Stats(time.Minute)
	s.Resize(10, 10, 100) // must not panic
}

func TestConcurrentAdd(t *testing.T) {
	s := New(200, 50, 1<<20)
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Add(Record{Route: "r", ReqBody: []byte("x")})
				s.AddEvent(Event{Level: "WARN"})
			}
		}()
	}
	wg.Wait()
	ov := s.Overview(0)
	if ov.Total != 20*200 {
		t.Fatalf("total = %d, want %d", ov.Total, 20*200)
	}
}

func TestResizeKeepsRecentRecords(t *testing.T) {
	s := New(5, 5, 1<<20)
	var last uint64
	for i := 0; i < 5; i++ {
		last = s.Add(Record{Route: "r"})
	}
	s.Resize(2, 2, 1<<20)
	list := s.List(Filter{})
	if len(list) != 2 {
		t.Fatalf("len after resize = %d, want 2", len(list))
	}
	if list[0].ID != last {
		t.Fatalf("most recent record lost across resize: got %d want %d", list[0].ID, last)
	}
}

func TestStatsPercentiles(t *testing.T) {
	s := New(100, 10, 1<<20)
	for _, d := range []int64{10, 20, 30, 40, 100} {
		s.Add(Record{Route: "r", Status: 200, Duration: d})
	}
	st := s.Stats(0)
	if st.Count != 5 {
		t.Fatalf("count = %d", st.Count)
	}
	if st.P50Ms == 0 {
		t.Fatal("p50 should be nonzero")
	}
}
