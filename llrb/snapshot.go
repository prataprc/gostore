// LLRB MVCC snapshot readers.

package llrb

import "sync/atomic"
import "time"
import "bytes"
import "strings"
import "math"
import "io"
import "fmt"
import "strconv"
import "runtime/debug"

import "github.com/prataprc/storage.go/lib"
import "github.com/prataprc/storage.go/log"
import "github.com/prataprc/storage.go/api"

//---- snapshot ticker

func (writer *LLRBWriter) snapshotticker(interval int64, finch chan bool) {
	llrb := writer.llrb
	tick := time.NewTicker(time.Duration(interval) * time.Millisecond)

	defer func() {
		if r := recover(); r != nil {
			log.Errorf("%v snapshotticker() crashed: %v\n", llrb.logprefix, r)
			log.Errorf("\n%s", lib.GetStacktrace(2, debug.Stack()))
			// TODO: what if there are active snapshots and active iterators.
			llrb.Destroy()
		}
		tick.Stop()
	}()

loop:
	for {
		<-tick.C
		id := strconv.Itoa(int(time.Now().UnixNano() >> 19))
		select { // break out if writer has exited
		case <-finch:
			break loop
		default:
		}
		log.Tracef("%v snapshot tick for $%v ...\n", llrb.logprefix, id)
		if err := writer.makeSnapshot(id); err != nil {
			log.Errorf("%v make snapshot $%v failed: %v\n", llrb.logprefix, id, err)
			break loop
		}
	}
}

// LLRBSnapshot holds on to a read-only version of the LLRB tree.
type LLRBSnapshot struct {
	// 64-bit aligned reader statistics
	n_lookups   int64
	n_ranges    int64
	n_cclookups int64
	n_ccranges  int64

	// 64-bit aligned writer statistics
	n_count       int64
	n_inserts     int64
	n_updates     int64
	n_deletes     int64
	n_nodes       int64
	n_frees       int64
	n_clones      int64
	n_reclaims    int64
	keymemory     int64
	valmemory     int64
	h_upsertdepth lib.HistogramInt64

	// can be unaligned fields

	llrb  *LLRB
	id    string
	root  *Llrbnode
	dead  bool
	clock *vectorclock

	// snapshot specific fields
	reclaim  []*Llrbnode
	next     *LLRBSnapshot
	refcount int64

	// settings
	fmask     metadataMask
	logprefix string
}

// newsnapshot mvcc version for LLRB tree.
func (llrb *LLRB) newsnapshot(id string) *LLRBSnapshot {
	snapshot := &LLRBSnapshot{
		llrb:  llrb,
		id:    id,
		root:  llrb.root,
		dead:  llrb.dead,
		clock: llrb.clock.clone(),
		fmask: llrb.fmask,
		// writer statistics
		n_count:       llrb.n_count,
		n_inserts:     llrb.n_inserts,
		n_updates:     llrb.n_updates,
		n_deletes:     llrb.n_deletes,
		n_nodes:       llrb.n_nodes,
		n_frees:       llrb.n_frees,
		n_clones:      llrb.n_clones,
		n_reclaims:    llrb.mvcc.n_reclaims,
		keymemory:     llrb.keymemory,
		valmemory:     llrb.valmemory,
		h_upsertdepth: *llrb.h_upsertdepth,
	}
	snapshot.logprefix = fmt.Sprintf("[LLRBSnapshot-%s/%s]", llrb.name, id)

	snapshot.reclaim = make([]*Llrbnode, len(llrb.mvcc.reclaim))
	copy(snapshot.reclaim, llrb.mvcc.reclaim)
	llrb.mvcc.reclaim = llrb.mvcc.reclaim[:0] // reset writer reclaims

	// track to the tail of read-snapshot list.
	if llrb.mvcc.snapshot == nil {
		llrb.mvcc.snapshot = snapshot
	} else {
		parent := llrb.mvcc.snapshot
		for parent.next != nil {
			parent = parent.next
		}
		parent.next = snapshot
	}

	fmsg := "%v snapshot BORN %v nodes to reclaim...\n"
	log.Debugf(fmsg, snapshot.logprefix, len(snapshot.reclaim))
	llrb.mvcc.n_snapshots++
	llrb.mvcc.n_activess++
	return snapshot
}

//---- IndexSnapshot{} interface.

// ID implement IndexSnapshot{} interface.
func (snapshot *LLRBSnapshot) ID() string {
	return snapshot.id
}

// Count implement IndexSnapshot{} interface.
func (snapshot *LLRBSnapshot) Count() int64 {
	return snapshot.n_count
}

// Isactive implement IndexSnapshot{} interface.
func (snapshot *LLRBSnapshot) Isactive() bool {
	return snapshot.dead == false
}

// Refer implement IndexSnapshot interface.
func (snapshot *LLRBSnapshot) Refer() {
	log.Debugf("%v snapshot REF\n", snapshot.logprefix)
	atomic.AddInt64(&snapshot.refcount, 1)
}

// Release implement IndexSnapshot interface.
func (snapshot *LLRBSnapshot) Release() {
	log.Debugf("%v snapshot DEREF\n", snapshot.logprefix)
	refcount := atomic.AddInt64(&snapshot.refcount, -1)
	if refcount < 0 {
		panic("Release(): snapshot refcount gone negative")
	}
}

// Validate implement IndexSnapshot interface.
func (snapshot *LLRBSnapshot) Validate() {
	snapshot.validate(snapshot.root)
}

// Dotdump to convert whole tree into dot script that can be visualized using
// graphviz.
func (snapshot *LLRBSnapshot) Dotdump(buffer io.Writer) {
	lines := []string{
		"digraph llrb {",
		"  node[shape=record];",
		"}",
	}
	buffer.Write([]byte(strings.Join(lines[:len(lines)-1], "\n")))
	nd := snapshot.root
	nd.dotdump(buffer)
	buffer.Write([]byte(lines[len(lines)-1]))
}

//---- IndexReader{} interface.

// Has implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Has(key []byte) bool {
	return snapshot.Get(key, nil)
}

// Get implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Get(key []byte, callb api.NodeCallb) bool {
	defer func() {
		if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
			atomic.AddInt64(&snapshot.n_cclookups, 1)
		} else {
			atomic.AddInt64(&snapshot.n_lookups, 1)
		}
	}()

	if nd := snapshot.get(key); nd != nil {
		if callb == nil {
			return true
		}
		return callb(snapshot.llrb, 0, nd, nd)
	}
	return false
}

func (snapshot *LLRBSnapshot) get(key []byte) api.Node {
	mdsize, nd := snapshot.llrb.mdsize, snapshot.root
	for nd != nil {
		if nd.gtkey(mdsize, key, false) {
			nd = nd.left
		} else if nd.ltkey(mdsize, key, false) {
			nd = nd.right
		} else {
			return nd
		}
	}
	return nil // key is not present in the tree
}

// Min implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Min(callb api.NodeCallb) bool {
	defer func() {
		if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
			atomic.AddInt64(&snapshot.n_cclookups, 1)
		} else {
			atomic.AddInt64(&snapshot.n_lookups, 1)
		}
	}()

	if nd := snapshot.min(); nd != nil {
		if callb == nil {
			return true
		}
		return callb(snapshot.llrb, 0, nd, nd)
	}
	return false
}

func (snapshot *LLRBSnapshot) min() api.Node {
	var nd *Llrbnode
	if nd = snapshot.root; nd == nil {
		return nil
	}
	for nd.left != nil {
		nd = nd.left
	}
	return nd
}

// Max implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Max(callb api.NodeCallb) bool {
	defer func() {
		if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
			atomic.AddInt64(&snapshot.n_cclookups, 1)
		} else {
			atomic.AddInt64(&snapshot.n_lookups, 1)
		}
	}()

	if nd := snapshot.max(); nd != nil {
		if callb == nil {
			return true
		}
		return callb(snapshot.llrb, 0, nd, nd)
	}
	return false
}

func (snapshot *LLRBSnapshot) max() api.Node {
	var nd *Llrbnode
	if nd = snapshot.root; nd == nil {
		return nil
	}
	for nd.right != nil {
		nd = nd.right
	}
	return nd
}

// Range implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Range(lkey, hkey []byte, incl string, reverse bool, callb api.NodeCallb) {
	lkey, hkey = snapshot.llrb.fixrangeargs(lkey, hkey)
	if lkey != nil && hkey != nil && bytes.Compare(lkey, hkey) == 0 {
		if incl == "none" {
			return
		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}

	if reverse {
		switch incl {
		case "both":
			snapshot.llrb.rvrslehe(snapshot.root, lkey, hkey, callb)
		case "high":
			snapshot.llrb.rvrsleht(snapshot.root, lkey, hkey, callb)
		case "low":
			snapshot.llrb.rvrslthe(snapshot.root, lkey, hkey, callb)
		default:
			snapshot.llrb.rvrsltht(snapshot.root, lkey, hkey, callb)
		}

	} else {
		switch incl {
		case "both":
			snapshot.llrb.rangehele(snapshot.root, lkey, hkey, callb)
		case "high":
			snapshot.llrb.rangehtle(snapshot.root, lkey, hkey, callb)
		case "low":
			snapshot.llrb.rangehelt(snapshot.root, lkey, hkey, callb)
		default:
			snapshot.llrb.rangehtlt(snapshot.root, lkey, hkey, callb)
		}
	}

	if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
		atomic.AddInt64(&snapshot.n_ccranges, 1)
	} else {
		atomic.AddInt64(&snapshot.n_ranges, 1)
	}
}

// Iterate implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Iterate(lkey, hkey []byte, incl string, r bool) api.IndexIterator {
	lkey, hkey = snapshot.llrb.fixrangeargs(lkey, hkey)
	if lkey != nil && hkey != nil && bytes.Compare(lkey, hkey) == 0 {
		if incl == "none" {
			return nil
		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}

	llrb := snapshot.llrb
	var iter *iterator
	select {
	case iter = <-llrb.iterpool:
	default:
		iter = &iterator{}
	}

	// NOTE: always re-initialize, because we are getting it back from pool.
	iter.tree, iter.llrb = snapshot, llrb
	iter.nodes, iter.index, iter.limit = iter.nodes[:0], 0, 5
	iter.continuate = false
	iter.startkey, iter.endkey, iter.incl, iter.reverse = lkey, hkey, incl, r
	iter.closed, iter.activeiter = false, &llrb.activeiter

	if iter.nodes == nil {
		iter.nodes = make([]api.Node, 0)
	}

	iter.rangefill()
	if r {
		switch iter.incl {
		case "none":
			iter.incl = "high"
		case "low":
			iter.incl = "both"
		}
	} else {
		switch iter.incl {
		case "none":
			iter.incl = "low"
		case "high":
			iter.incl = "both"
		}
	}

	if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
		atomic.AddInt64(&snapshot.n_ccranges, 1)
	} else {
		atomic.AddInt64(&snapshot.n_ranges, 1)
	}
	atomic.AddInt64(&llrb.activeiter, 1)
	return iter
}

// don't use llrb.validate(), it will access stats that needs
// to be serialized.
func (snapshot *LLRBSnapshot) validate(root *Llrbnode) {
	llrb := snapshot.llrb

	h := lib.NewhistorgramInt64(1, 256, 1)
	_, km, vm := llrb.validatetree(root, isred(root), 0 /*blck*/, 1 /*dep*/, h)
	if km != snapshot.keymemory {
		fmsg := "validate(): keymemory:%v != actual:%v"
		panic(fmt.Errorf(fmsg, snapshot.keymemory, km))
	} else if vm != snapshot.valmemory {
		fmsg := "validate(): valmemory:%v != actual:%v"
		panic(fmt.Errorf(fmsg, snapshot.valmemory, vm))
	}

	// `h_height`.max should not exceed certain limit
	llrb.validatetree(root, isred(root), 0 /*blacks*/, 1 /*depth*/, h)
	if h.Samples() > 8 {
		nf := float64(snapshot.Count())
		if float64(h.Max()) > (3 * math.Log2(nf)) {
			fmsg := "validate(): max height %v exceeds log2(snapshot.count) %v"
			panic(fmt.Errorf(fmsg, float64(h.Max()), nf))
		}
	}

	snapshot.validatestats()
}

func (snapshot *LLRBSnapshot) validatestats() {
	// n_count should match (n_inserts - n_deletes)
	n_count := snapshot.n_count
	n_inserts, n_deletes := snapshot.n_inserts, snapshot.n_deletes
	if n_count != (n_inserts - n_deletes) {
		fmsg := "sstats(): n_count:%v != (n_inserts:%v - n_deletes:%v)"
		panic(fmt.Errorf(fmsg, n_count, n_inserts, n_deletes))
	}
	// n_nodes should match n_inserts
	n_nodes := snapshot.n_nodes
	if n_inserts != n_nodes {
		fmsg := "sstats(): n_inserts:%v != n_nodes:%v"
		panic(fmt.Errorf(fmsg, n_inserts, n_nodes))
	}
	// n_count should match ((n_nodes + n_clones - n_reclaims)
	n_clones, n_reclaims := snapshot.n_clones, snapshot.n_reclaims
	if n_count != (n_nodes + n_clones - n_reclaims) {
		fmsg := "sstats(): n_count:%v != (n_nodes:%v + n_clones:%v - reclaims:%v)"
		panic(fmt.Errorf(fmsg, n_count, n_nodes, n_clones, n_reclaims))
	}

	// n_deletes should match (n_reclaims - n_clones)
	if n_deletes != (n_reclaims - n_clones) {
		fmsg := "sstats(): n_deletes:%v != (n_reclaims:%v - n_clones:%v)"
		panic(fmt.Errorf(fmsg, n_deletes, n_reclaims, n_clones))
	}
	return
}
