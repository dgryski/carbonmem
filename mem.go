// memory-backed carbon store: speaks graphite in, zipper out
package carbonmem

import (
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/armon/go-radix"
)

// Whisper is an in-memory whisper-like store
type Whisper struct {
	sync.RWMutex
	t0     int32
	agg    int32
	idx    int
	epochs []map[int]uint64

	known map[int]int
	keys  *radix.Tree

	l *lookup
}

func NewWhisper(t0 int32, cap int, agg int32) *Whisper {

	t0 = t0 - (t0 % agg)

	epochs := make([]map[int]uint64, cap)
	epochs[0] = make(map[int]uint64)

	return &Whisper{
		t0:     t0,
		agg:    agg,
		epochs: epochs,
		known:  make(map[int]int),
		keys:   radix.New(),
		l:      newLookup(),
	}
}

func (w *Whisper) Set(t int32, metric string, val uint64) {

	w.Lock()
	defer w.Unlock()

	// based on github.com/dgryski/go-timewindow

	t = t - (t % w.agg)

	if t == w.t0 {

		id := w.l.FindOrAdd(metric)

		m := w.epochs[w.idx]

		// have we seen this metric this epoch?
		_, ok := m[id]
		if !ok {
			// one more occurrence of this metric
			v, ok := w.known[id]
			if !ok {
				// new key, add to prefix list
				w.keys.Insert(metric, struct{}{})
			}
			w.known[id] = v + 1
		}

		m[id] = val
		return
	}

	if t > w.t0 {
		// advance the buffer, decrementing counts for all entries in the
		// maps we pass by

		for w.t0 < t {
			w.t0 += w.agg
			w.idx++
			if w.idx >= len(w.epochs) {
				w.idx = 0
			}

			m := w.epochs[w.idx]
			if m != nil {
				for id, _ := range m {
					w.known[id]--
					if w.known[id] == 0 {
						delete(w.known, id)
						w.keys.Delete(w.l.Reverse(id))
					}
				}
				w.epochs[w.idx] = nil
			}
		}

		id := w.l.FindOrAdd(metric)

		v, ok := w.known[id]
		if !ok {
			w.keys.Insert(metric, struct{}{})
		}
		w.known[id] = v + 1
		w.epochs[w.idx] = map[int]uint64{id: val}
		return
	}

	// TODO(dgryski): limit how far back we can update

	// less common -- update the past
	back := int((w.t0 - t) / w.agg)

	if back >= len(w.epochs) {
		// too far in the past, ignore
		return
	}

	idx := w.idx - back

	if idx < 0 {
		// need to wrap around
		idx += len(w.epochs)
	}

	m := w.epochs[idx]
	if m == nil {
		m = make(map[int]uint64)
		w.epochs[idx] = m
	}

	id := w.l.FindOrAdd(metric)

	_, ok := m[id]
	if !ok {
		w.known[id]++
	}
	m[id] = val
}

type Fetched struct {
	From   int32
	Until  int32
	Step   int32
	Values []float64
}

func (w *Whisper) Fetch(metric string, from int32, until int32) *Fetched {

	w.RLock()
	defer w.RUnlock()

	// round to window
	from = from - (from % w.agg)
	until = until - (until % w.agg)

	if from > w.t0 {
		return nil
	}

	id, ok := w.l.Find(metric)
	if !ok {
		// unknown metric
		return nil
	}

	if _, ok := w.known[id]; !ok {
		return nil
	}

	if until < from {
		return nil
	}

	if min := w.t0 - int32(len(w.epochs))*w.agg + 1; from < min {
		from = min
	}

	idx := w.idx - int((w.t0-from)/w.agg)
	if idx < 0 {
		idx += len(w.epochs)
	}

	points := (until - from + (w.agg - 1) + 1) / w.agg // inclusive of 'until'
	r := &Fetched{
		From:   from,
		Until:  until,
		Step:   w.agg,
		Values: make([]float64, points),
	}

	l := len(w.epochs)

	for p, t := 0, idx; p < int(points); p, t = p+1, t+1 {
		if t >= l {
			t = 0
		}

		m := w.epochs[t]
		if v, ok := m[id]; ok {
			r.Values[p] = float64(v)
		} else {
			r.Values[p] = math.NaN()
		}
	}

	return r
}

type Glob struct {
	Metric string
	IsLeaf bool
}

type globByName []Glob

func (g globByName) Len() int {
	return len(g)
}

func (g globByName) Swap(i, j int) {
	g[i], g[j] = g[j], g[i]
}

func (g globByName) Less(i, j int) bool {
	return g[i].Metric < g[j].Metric
}

// TODO(dgryski): only does prefix matching for the moment

func (w *Whisper) Find(query string) []Glob {

	w.RLock()
	defer w.RUnlock()

	query = strings.TrimSuffix(query, "*")

	var response []Glob
	l := len(query)
	seen := make(map[string]bool)
	w.keys.WalkPrefix(query, func(k string, v interface{}) bool {
		// figure out if we're a leaf or not
		dot := strings.IndexByte(k[l:], '.')
		var leaf bool
		m := k
		if dot == -1 {
			leaf = true
		} else {
			m = k[:dot+l]
		}
		if !seen[m] {
			seen[m] = true
			response = append(response, Glob{Metric: m, IsLeaf: leaf})
		}
		// false == "don't terminate iteration"
		return false
	})

	sort.Sort(globByName(response))

	return response
}

type keysByCount struct {
	keys   []int
	counts map[int]uint64
}

func (k keysByCount) Len() int {
	return len(k.keys)
}

func (k keysByCount) Swap(i, j int) {
	k.keys[i], k.keys[j] = k.keys[j], k.keys[i]
}

func (k keysByCount) Less(i, j int) bool {
	// actually "GreaterThan"
	return k.counts[k.keys[i]] > k.counts[k.keys[j]]
}

func (w *Whisper) TopK(prefix string, seconds int32) []Glob {

	w.RLock()
	defer w.RUnlock()

	buckets := int(seconds / w.agg)

	idx := w.idx
	l := len(w.epochs)

	idx -= buckets - 1
	if idx < 0 {
		idx += l
	}

	// gather counts for all metrics in this time period
	counts := make(map[int]uint64)
	for i := 0; i < buckets; i++ {
		m := w.epochs[idx]
		for id, v := range m {
			k := w.l.Reverse(id)
			if strings.HasPrefix(k, prefix) {
				counts[id] += v
			}
		}
		idx++
		if idx >= l {
			idx = 0
		}
	}

	var keys []int
	for k, _ := range counts {
		keys = append(keys, k)
	}

	countedKeys := keysByCount{keys: keys, counts: counts}

	sort.Sort(countedKeys)
	var response []Glob

	for i := 0; i < 100 && i < countedKeys.Len(); i++ {
		m := w.l.Reverse(countedKeys.keys[i])
		response = append(response, Glob{Metric: m, IsLeaf: true})
	}

	return response
}

type lookup struct {
	keys    map[string]int
	revKeys map[int]string
	numKeys int
}

func newLookup() *lookup {
	return &lookup{
		keys:    make(map[string]int),
		revKeys: make(map[int]string),
	}
}

func (l *lookup) Find(key string) (int, bool) {
	id, ok := l.keys[key]
	return id, ok
}

func (l *lookup) FindOrAdd(key string) int {

	id, ok := l.keys[key]

	if ok {
		return id
	}

	id = l.numKeys
	l.numKeys++

	l.keys[key] = id
	l.revKeys[id] = key

	return id
}

func (l *lookup) Reverse(id int) string {

	key, ok := l.revKeys[id]

	if !ok {
		panic("looked up invalid key")
	}

	return key
}
