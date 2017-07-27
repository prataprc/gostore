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

import "github.com/prataprc/gostore/lib"
import "github.com/prataprc/golog"
import "github.com/prataprc/gostore/api"

// go-routine to generate snapshots.
func (writer *LLRBWriter) snapshotticker(interval int64, finch chan bool) {
	llrb := writer.llrb
	tick := time.NewTicker(time.Duration(interval) * time.Millisecond)

	defer func() {
		if r := recover(); r != nil {
			log.Errorf("%v snapshotticker() crashed: %v\n", llrb.logprefix, r)
			log.Errorf("\n%s", lib.GetStacktrace(2, debug.Stack()))
			for err := llrb.Destroy(); err != nil; { // handle active iterators.
				err = llrb.Destroy()
				time.Sleep(writer.tryexitafter /* 100ms ? */)
			}
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
			if err.Error() != "closed" {
				fmsg := "%v make snapshot $%v failed: %v\n"
				log.Errorf(fmsg, llrb.logprefix, id, err)
			}
			break loop
		}
	}
}

// LLRBSnapshot holds on to a read-only version of the LLRB tree.
type LLRBSnapshot struct {
	refcount int64
	llrbstats

	// can be unaligned fields

	llrb  *LLRB
	id    string // snapshot id
	root  *Llrbnode
	clock api.Clock
	dead  bool

	// snapshot specific fields
	reclaim []*Llrbnode
	next    *LLRBSnapshot

	// settings
	logprefix string
}

// newsnapshot mvcc version for LLRB tree.
func newsnapshot(llrb *LLRB, id string) *LLRBSnapshot {
	snapshot := &LLRBSnapshot{
		llrb:  llrb,
		id:    id,
		root:  llrb.getroot(),
		clock: llrb.Getclock(),
		dead:  llrb.dead,
		// writer statistics
		llrbstats: llrb.llrbstats,
	}
	snapshot.logprefix = fmt.Sprintf("[LLRBSnapshot-%s/%s]", llrb.name, id)

	snapshot.reclaim = make([]*Llrbnode, len(llrb.mvcc.reclaim))
	copy(snapshot.reclaim, llrb.mvcc.reclaim)
	llrb.mvcc.reclaim = llrb.mvcc.reclaim[:0] // reset writer reclaims

	// track to the tail of snapshot list.
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
	atomic.AddInt64(&llrb.n_snapshots, 1)
	atomic.AddInt64(&llrb.n_activess, 1)
	return snapshot
}

// return the sum of all nodes that needs to be reclaimed from snapshots.
func countreclaimnodes(head *LLRBSnapshot) (total int64) {
	for snapshot := head; snapshot != nil; snapshot = snapshot.next {
		total += int64(len(snapshot.reclaim))
	}
	return total
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

// Getclock implement IndexSnapshot{} inteface.
func (snapshot *LLRBSnapshot) Getclock() api.Clock {
	return snapshot.clock
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
	if refcount == 0 {
		if err := snapshot.llrb.mvcc.writer.purgeSnapshot(); err != nil {
			log.Errorf("%v purgeSnapshot(): %v\n", snapshot.logprefix, err)
		}
	} else if refcount < 0 {
		panic("Release(): snapshot refcount gone negative")
	}
}

// Metadata implement api.Index{} interface.
func (snapshot *LLRBSnapshot) Metadata() []byte {
	return nil
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
	defer snapshot.countlookup(atomic.LoadInt64(&snapshot.llrb.mvcc.ismut))
	_, ok := doget(snapshot.llrb, snapshot.root, key, callb)
	return ok
}

// Min implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Min(callb api.NodeCallb) bool {
	defer func() {
		atomic.AddInt64(&snapshot.n_lookups, 1)
		if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
			atomic.AddInt64(&snapshot.n_cclookups, 1)
		}
	}()

	if nd, _ := snapshot.min(snapshot.root); nd == nil {
		if callb != nil {
			callb(snapshot.llrb, 0, nil, nil, api.ErrorKeyMissing)
		}
		return false
	} else if callb != nil {
		callb(snapshot.llrb, 0, nd, nd, nil)
	}
	return true
}

func (snapshot *LLRBSnapshot) min(nd *Llrbnode) (api.Node, bool) {
	if nd == nil {
		return nil, false
	} else if minnd, ok := snapshot.min(nd.left); ok {
		return minnd, ok
	} else if nd.IsDeleted() {
		return snapshot.min(nd.right)
	}
	return nd, true
}

// Max implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Max(callb api.NodeCallb) bool {
	defer func() {
		atomic.AddInt64(&snapshot.n_lookups, 1)
		if atomic.LoadInt64(&snapshot.llrb.mvcc.ismut) == 1 {
			atomic.AddInt64(&snapshot.n_cclookups, 1)
		}
	}()

	if nd, _ := snapshot.max(snapshot.root); nd == nil {
		if callb != nil {
			callb(snapshot.llrb, 0, nil, nil, api.ErrorKeyMissing)
		}
		return false
	} else if callb != nil {
		callb(snapshot.llrb, 0, nd, nd, nil)
	}
	return true
}

func (snapshot *LLRBSnapshot) max(nd *Llrbnode) (api.Node, bool) {
	if nd == nil {
		return nil, false
	} else if maxnd, ok := snapshot.max(nd.right); ok {
		return maxnd, ok
	} else if nd.IsDeleted() {
		return snapshot.max(nd.left)
	}
	return nd, true
}

// Range implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Range(
	lkey, hkey []byte, incl string, reverse bool, callb api.NodeCallb) {

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
	}
	atomic.AddInt64(&snapshot.n_ranges, 1)
}

// Iterate implement IndexReader{} interface.
func (snapshot *LLRBSnapshot) Iterate(
	lkey, hkey []byte, incl string, r bool) api.IndexIterator {

	lkey, hkey = snapshot.llrb.fixrangeargs(lkey, hkey)
	if lkey != nil && hkey != nil && bytes.Compare(lkey, hkey) == 0 {
		if incl == "none" {
			return nil
		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}

	llrb := snapshot.llrb

	// NOTE: always re-initialize, because we are getting it back from pool.
	iter := llrb.getiterator()
	iter.tree, iter.llrb = snapshot, llrb
	iter.continuate = false
	iter.nodes, iter.index, iter.limit = iter.nodes[:0], 0, int(llrb.maxlimit)
	// startkey
	iter.startkey = lib.Fixbuffer(iter.startkey, llrb.maxkeysize)
	n := copy(iter.startkey, lkey)
	iter.startkey = iter.startkey[:n]
	// endkey
	iter.endkey = lib.Fixbuffer(iter.endkey, llrb.maxkeysize)
	n = copy(iter.endkey, hkey)
	iter.endkey = iter.endkey[:n]
	// other params
	iter.incl, iter.reverse = incl, r
	iter.closed, iter.n_activeiter = false, &llrb.n_activeiter

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
	}
	atomic.AddInt64(&snapshot.n_ranges, 1)
	atomic.AddInt64(&llrb.n_activeiter, 1)
	return iter
}

// Validate the following expectation on the snapshot.
//  * Walk the tree and calculate cummulative memory consumed by
//    all keys, confirm the same with keymemory accounting.
//  * Walk the tree and calculate cummulative memory consumed by
//    all values, confirm the same with valmemory accounting.
//  * Maximum height of the tree should not exceed 3*math.Log2(n),
//    where `n` is the number of entries on the snapshot.
//  * Validatestats.
func (snapshot *LLRBSnapshot) validate(root *Llrbnode) {
	llrb, h := snapshot.llrb, lib.NewhistorgramInt64(1, 256, 1)
	_, km, vm := llrb.validatetree(root, isred(root), 0 /*blcks*/, 1 /*dep*/, h)
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

// validatestats can be used for quick validation based on
// statistic accounting.
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
