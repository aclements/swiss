// Copyright 2024 The Cockroach Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package swiss

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// TODO(peter):
// - Add metamorphic tests that cross-check behavior at various bucket sizes.
// - Add fuzz testing.

// toBuiltinMap returns the elements as a map[K]V. Useful for testing.
func (m *Map[K, V]) toBuiltinMap() map[K]V {
	r := make(map[K]V)
	m.All(func(k K, v V) bool {
		r[k] = v
		return true
	})
	return r
}

// TODO(peter): Extracting a random element might be generally useful. Should
// this be promoted to the public API? Note that the elements are not selected
// uniformly randomly. If we promote this method to the public API it should
// take a rand.Rand.
func (m *Map[K, V]) randElement() (key K, value V, ok bool) {
	// Rely on random iteration order to give us a random element.
	m.All(func(k K, v V) bool {
		key, value = k, v
		ok = true
		return false
	})
	return
}

func TestLittleEndian(t *testing.T) {
	// The implementation of group h2 matching and group empty and deleted
	// masking assumes a little endian CPU architecture. Assert that we are
	// running on one.
	b := []uint8{0x1, 0x2, 0x3, 0x4}
	v := *(*uint32)(unsafe.Pointer(&b[0]))
	require.EqualValues(t, 0x04030201, v)
}

func TestProbeSeq(t *testing.T) {
	genSeq := func(n int, hash, mask uintptr) []uintptr {
		seq := makeProbeSeq(hash, mask)
		vals := make([]uintptr, n)
		for i := 0; i < n; i++ {
			vals[i] = seq.offset
			seq = seq.next()
		}
		return vals
	}
	genGroups := func(n int, start, count uintptr) []uintptr {
		var vals []uintptr
		for i := start % groupSize; i < count; i += groupSize {
			vals = append(vals, i)
		}
		sort.Slice(vals, func(i, j int) bool {
			return vals[i] < vals[j]
		})
		return vals
	}

	// The Abseil probeSeq test cases.
	expected := []uintptr{0, 8, 24, 48, 80, 120, 40, 96, 32, 104, 56, 16, 112, 88, 72, 64}
	require.Equal(t, expected, genSeq(16, 0, 127))
	require.Equal(t, expected, genSeq(16, 128, 127))

	// Verify that we touch all of the groups no matter what our start offset
	// within the group is.
	for i := uintptr(0); i < 128; i++ {
		vals := genSeq(16, i, 127)
		require.Equal(t, 16, len(vals))
		sort.Slice(vals, func(i, j int) bool {
			return vals[i] < vals[j]
		})
		require.Equal(t, genGroups(16, i, 128), vals)
	}
}

func TestMatchH2(t *testing.T) {
	ctrls := []ctrl{0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8}
	for i := uintptr(1); i <= 8; i++ {
		match := makeCtrlBytes(ctrls).GroupAt(0).matchH2(i)
		bit := match.first()
		require.EqualValues(t, i-1, bit)
	}
}

func TestMatchEmpty(t *testing.T) {
	testCases := []struct {
		ctrls    []ctrl
		expected []uintptr
	}{
		{[]ctrl{0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8}, nil},
		{[]ctrl{0x1, 0x2, 0x3, ctrlEmpty, 0x5, ctrlDeleted, 0x7, ctrlSentinel}, []uintptr{3}},
		{[]ctrl{0x1, 0x2, 0x3, ctrlEmpty, 0x5, 0x6, ctrlEmpty, 0x8}, []uintptr{3, 6}},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			match := makeCtrlBytes(c.ctrls).GroupAt(0).matchEmpty()
			var results []uintptr
			for match != 0 {
				idx := match.first()
				results = append(results, idx)
				match = match.remove(idx)
			}
			require.Equal(t, c.expected, results)
		})
	}
}

func TestMatchEmptyOrDeleted(t *testing.T) {
	testCases := []struct {
		ctrls    []ctrl
		expected []uintptr
	}{
		{[]ctrl{0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8}, nil},
		{[]ctrl{0x1, 0x2, ctrlEmpty, ctrlDeleted, 0x5, 0x6, 0x7, ctrlSentinel}, []uintptr{2, 3}},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			match := makeCtrlBytes(c.ctrls).GroupAt(0).matchEmptyOrDeleted()
			var results []uintptr
			for match != 0 {
				idx := match.first()
				results = append(results, idx)
				match = match.remove(idx)
			}
			require.Equal(t, c.expected, results)
		})
	}
}

func TestConvertNonFullToEmptyAndFullToDeleted(t *testing.T) {
	ctrls := make([]ctrl, groupSize)
	expected := make([]ctrl, groupSize)
	for i := 0; i < 100; i++ {
		for j := 0; j < groupSize; j++ {
			switch rand.Intn(4) {
			case 0: // 25% empty
				ctrls[j] = ctrlEmpty
				expected[j] = ctrlEmpty
			case 1: // 25% deleted
				ctrls[j] = ctrlDeleted
				expected[j] = ctrlEmpty
			case 2: // 25% sentinel
				ctrls[j] = ctrlSentinel
				expected[j] = ctrlEmpty
			default: // 25% full
				ctrls[j] = ctrl(rand.Intn(127))
				expected[j] = ctrlDeleted
			}
		}

		makeCtrlBytes(ctrls).GroupAt(0).convertNonFullToEmptyAndFullToDeleted()
		require.EqualValues(t, expected, ctrls)
	}
}

func TestAbsentAt(t *testing.T) {
	testCases := []struct {
		set   string
		start uintptr
		end   uintptr
	}{
		{set: "01001100", start: 1, end: 2},
		{set: "11001000", start: 0, end: 3},
		{set: "00001001", start: 4, end: 0},
		{set: "10001001", start: 0, end: 0},
		{set: "00000000", start: 8, end: 8},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			b := bitsetFromString(t, c.set)
			require.Equal(t, c.start, b.absentAtStart())
			require.Equal(t, c.end, b.absentAtEnd())
		})
	}
}

func bitsetFromString(t *testing.T, str string) bitset {
	require.Equal(t, 8, len(str))
	var b bitset
	for i := 0; i < 8; i++ {
		require.True(t, str[i] == '0' || str[i] == '1')
		if str[i] == '1' {
			b |= 0x80 << (i * 8)
		}
	}
	return b
}

func TestWasNeverFull(t *testing.T) {
	b := &bucket[int, int]{
		capacity: 15,
		ctrls:    makeCtrlBytes(make([]ctrl, 16)),
	}

	testCases := []struct {
		emptyIndexes []uintptr
		expected     bool
	}{
		{[]uintptr{}, false},
		{[]uintptr{0}, false},
		{[]uintptr{0, 15}, true},
		{[]uintptr{1, 15}, true},
		{[]uintptr{2, 15}, true},
		{[]uintptr{3, 15}, true},
		{[]uintptr{4, 15}, true},
		{[]uintptr{5, 15}, true},
		{[]uintptr{6, 15}, true},
		{[]uintptr{7, 15}, true},
		{[]uintptr{8, 15}, false},
		{[]uintptr{0, 14}, true},
		{[]uintptr{0, 13}, true},
		{[]uintptr{0, 12}, true},
		{[]uintptr{0, 11}, true},
		{[]uintptr{0, 10}, true},
		{[]uintptr{0, 9}, true},
		{[]uintptr{0, 8}, true},
		{[]uintptr{0, 7}, false},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			for i := uintptr(0); i < 16; i++ {
				*b.ctrls.At(i) = 0
			}
			for _, i := range c.emptyIndexes {
				*b.ctrls.At(i) = ctrlEmpty
			}
			require.Equal(t, c.expected, b.wasNeverFull(0))
		})
	}
}

func TestInitialCapacity(t *testing.T) {
	testCases := []struct {
		initialCapacity   int
		maxBucketCapacity uintptr
		expectedCapacity  int
		expectedBuckets   uintptr
	}{
		{0, defaultMaxBucketCapacity, 0, 1},
		{1, defaultMaxBucketCapacity, 7, 1},
		{7, defaultMaxBucketCapacity, 7, 1},
		{8, defaultMaxBucketCapacity, 15, 1},
		{896, defaultMaxBucketCapacity, 1023, 1},
		{897, defaultMaxBucketCapacity, 2047, 1},
		{16, 7, 7 * 4, 4},
		{65536, 4095, 4095 * 32, 32},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			m := New[int, int](c.initialCapacity,
				WithMaxBucketCapacity[int, int](c.maxBucketCapacity))
			require.EqualValues(t, c.expectedBuckets, m.bucketCount())
			require.EqualValues(t, c.expectedCapacity, m.capacity())
		})
	}
}

func TestBasic(t *testing.T) {
	test := func(t *testing.T, m *Map[int, int]) {
		const count = 100

		e := make(map[int]int)
		require.EqualValues(t, 0, m.Len())
		require.EqualValues(t, 0, m.bucket0.growthLeft)

		// Non-existent.
		for i := 0; i < count; i++ {
			_, ok := m.Get(i)
			require.False(t, ok)
		}

		// Insert.
		for i := 0; i < count; i++ {
			m.Put(i, i+count)
			e[i] = i + count
			v, ok := m.Get(i)
			require.True(t, ok)
			require.EqualValues(t, i+count, v)
			require.EqualValues(t, i+1, m.Len())
			require.Equal(t, e, m.toBuiltinMap())
		}

		// Update.
		for i := 0; i < count; i++ {
			m.Put(i, i+2*count)
			e[i] = i + 2*count
			v, ok := m.Get(i)
			require.True(t, ok)
			require.EqualValues(t, i+2*count, v)
			require.EqualValues(t, count, m.Len())
			require.Equal(t, e, m.toBuiltinMap())
		}

		// Delete.
		for i := 0; i < count; i++ {
			m.Delete(i)
			delete(e, i)
			require.EqualValues(t, count-i-1, m.Len())
			_, ok := m.Get(i)
			require.False(t, ok)
			require.Equal(t, e, m.toBuiltinMap())
		}
	}

	t.Run("normal", func(t *testing.T) {
		test(t, New[int, int](0))
	})

	t.Run("degenerate", func(t *testing.T) {
		testDegenerate := func(t *testing.T, h uintptr) {
			m := New[int, int](0,
				WithHash[int, int](func(key *int, seed uintptr) uintptr {
					return h
				}),
				WithMaxBucketCapacity[int, int](7))
			test(t, m)
		}

		for _, v := range []uintptr{0, ^uintptr(0)} {
			t.Run(fmt.Sprintf("%016x", v), func(t *testing.T) {
				testDegenerate(t, v)
			})
		}
		for i := 0; i < 10; i++ {
			v := uintptr(rand.Uint64())
			t.Run(fmt.Sprintf("%016x", v), func(t *testing.T) {
				testDegenerate(t, v)
			})
		}
	})
}

func TestRandom(t *testing.T) {
	test := func(t *testing.T, m *Map[int, int]) {
		e := make(map[int]int)
		for i := 0; i < 10000; i++ {
			switch r := rand.Float64(); {
			case r < 0.5: // 50% inserts
				k, v := rand.Int(), rand.Int()
				m.Put(k, v)
				e[k] = v
			case r < 0.65: // 15% updates
				if k, _, ok := m.randElement(); !ok {
					require.EqualValues(t, 0, m.Len(), e)
				} else {
					v := rand.Int()
					m.Put(k, v)
					e[k] = v
				}
			case r < 0.80: // 15% deletes
				if k, _, ok := m.randElement(); !ok {
					require.EqualValues(t, 0, m.Len(), e)
				} else {
					m.Delete(k)
					delete(e, k)
				}
			case r < 0.95: // 25% lookups
				if k, v, ok := m.randElement(); !ok {
					require.EqualValues(t, 0, m.Len(), e)
				} else {
					require.EqualValues(t, e[k], v)
				}
			default: // 5% rehash in place and iterate
				m.bucket0.rehashInPlace(m)
				require.Equal(t, e, m.toBuiltinMap())
			}
			require.EqualValues(t, len(e), m.Len())
		}
	}

	t.Run("normal", func(t *testing.T) {
		test(t, New[int, int](0))
	})

	t.Run("degenerate", func(t *testing.T) {
		testDegenerate := func(t *testing.T, h uintptr) {
			m := New[int, int](0,
				WithHash[int, int](func(key *int, seed uintptr) uintptr {
					return h
				}),
				WithMaxBucketCapacity[int, int](512))
			test(t, m)
		}

		for _, v := range []uintptr{0, ^uintptr(0)} {
			t.Run(fmt.Sprintf("%016x", v), func(t *testing.T) {
				testDegenerate(t, v)
			})
		}
	})
}

func TestIterateMutate(t *testing.T) {
	m := New[int, int](0)
	for i := 0; i < 100; i++ {
		m.Put(i, i)
	}
	e := m.toBuiltinMap()
	require.EqualValues(t, 100, m.Len())
	require.EqualValues(t, 100, len(e))

	// Iterate over the map, resizing it periodically. We should see all of
	// the elements that were originally in the map because All takes a
	// snapshot of the ctrls and slots before iterating.
	vals := make(map[int]int)
	m.All(func(k, v int) bool {
		if (k % 10) == 0 {
			m.bucket0.resize(m, 2*m.bucket0.capacity+1)
		}
		vals[k] = v
		return true
	})
	require.EqualValues(t, e, vals)
}

func TestClear(t *testing.T) {
	testCases := []struct {
		count             int
		maxBucketCapacity uintptr
	}{
		{count: 1000, maxBucketCapacity: math.MaxUint64},
		{count: 1000, maxBucketCapacity: 7},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			m := New[int, int](0, WithMaxBucketCapacity[int, int](c.maxBucketCapacity))
			for i := 0; i < c.count; i++ {
				m.Put(i, i)
			}

			capacity := m.capacity()
			m.Clear()
			require.EqualValues(t, 0, m.Len())
			require.EqualValues(t, capacity, m.capacity())

			m.All(func(k, v int) bool {
				require.Fail(t, "should not iterate")
				return true
			})
		})
	}
}

type countingAllocator[K comparable, V any] struct {
	alloc int
	free  int
}

func (a *countingAllocator[K, V]) Alloc(ctrls, slots int) ([]uint8, []Slot[K, V]) {
	a.alloc++
	return make([]uint8, ctrls), make([]Slot[K, V], slots)
}

func (a *countingAllocator[K, V]) Free(_ []uint8, _ []Slot[K, V]) {
	a.free++
}

func TestAllocator(t *testing.T) {
	a := &countingAllocator[int, int]{}
	m := New[int, int](0, WithAllocator[int, int](a),
		WithMaxBucketCapacity[int, int](math.MaxUint64))

	for i := 0; i < 100; i++ {
		m.Put(i, i)
	}

	// 8 -> 16 -> 32 -> 64 -> 128
	const expected = 5
	require.EqualValues(t, expected, a.alloc)
	require.EqualValues(t, expected-1, a.free)

	m.Close()

	require.EqualValues(t, expected, a.free)
}

func TestResizeVsSplit(t *testing.T) {
	if invariants {
		t.Skip("skipped due to slowness under invariants")
	}

	count := 1_000_000 + rand.Intn(500_000)
	m := New[int, int](count, WithMaxBucketCapacity[int, int](0))
	for i, x := 0, 0; i < count; i++ {
		x += rand.Intn(128) + 1
		m.Put(x, x)
	}
	start := time.Now()
	m.bucket0.split(m)
	if testing.Verbose() {
		fmt.Printf(" split(%d): %6.3fms\n", count, time.Since(start).Seconds()*1000)
	}

	m = New[int, int](count, WithMaxBucketCapacity[int, int](math.MaxUint64))
	for i, x := 0, 0; i < count; i++ {
		x += rand.Intn(128) + 1
		m.Put(x, x)
	}
	start = time.Now()
	m.bucket0.resize(m, 2*m.bucket0.capacity+1)
	if testing.Verbose() {
		fmt.Printf("resize(%d): %6.3fms\n", count, time.Since(start).Seconds()*1000)
	}
}
