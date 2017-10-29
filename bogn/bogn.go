package bogn

import "fmt"
import "sync"
import "unsafe"
import "time"
import "strings"
import "strconv"
import "runtime"
import "io/ioutil"
import "sync/atomic"
import "encoding/json"

import "github.com/prataprc/gostore/api"
import "github.com/prataprc/gostore/lib"
import "github.com/prataprc/gostore/llrb"
import "github.com/prataprc/gostore/bubt"
import "github.com/prataprc/golog"
import s "github.com/prataprc/gosettings"

// TODO: enable count aggregation across snapshots, with data-structures
// that support LSM it is difficult to maintain accurate count.

// Bogn instance to index key,value pairs.
type Bogn struct {
	// atomic access, 8-byte aligned
	nroutines int64
	dgmstate  int64

	name     string
	snapshot unsafe.Pointer // *snapshot
	finch    chan struct{}
	snaprw   sync.RWMutex

	memstore    string
	dgm         bool
	workingset  bool
	ratio       float64
	period      time.Duration
	memcapacity int64
	setts       s.Settings
	logprefix   string
}

// New create a new bogn instance.
func New(name string, setts s.Settings) (*Bogn, error) {
	bogn := (&Bogn{name: name}).readsettings(setts)
	bogn.finch = make(chan struct{})
	bogn.logprefix = fmt.Sprintf("BOGN [%v]", name)

	disks, err := bogn.opendisksnaps(setts)
	if err != nil {
		bogn.Close()
		return nil, err
	}
	head, err := opensnapshot(bogn, disks)
	if err != nil {
		bogn.Close()
		return nil, err
	}
	head.refer()
	bogn.setheadsnapshot(head)

	log.Infof("%v started", bogn.logprefix)

	go purger(bogn)
	go compactor(bogn)

	return bogn, nil
}

func (bogn *Bogn) readsettings(setts s.Settings) *Bogn {
	bogn.memstore = setts.String("memstore")
	bogn.dgm = setts.Bool("dgm")
	bogn.workingset = setts.Bool("workingset")
	bogn.ratio = setts.Float64("ratio")
	bogn.period = time.Duration(setts.Int64("period")) * time.Second
	bogn.setts = setts

	atomic.StoreInt64(&bogn.dgmstate, 0)
	if bogn.dgm {
		atomic.StoreInt64(&bogn.dgmstate, 1)
	}

	switch bogn.memstore {
	case "llrb", "mvcc":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		bogn.memcapacity = llrbsetts.Int64("keycapacity")
		bogn.memcapacity += llrbsetts.Int64("valcapacity")
	}
	return bogn
}

func (bogn *Bogn) currsnapshot() *snapshot {
	return (*snapshot)(atomic.LoadPointer(&bogn.snapshot))
}

func (bogn *Bogn) setheadsnapshot(snapshot *snapshot) {
	atomic.StorePointer(&bogn.snapshot, unsafe.Pointer(snapshot))
}

func (bogn *Bogn) latestsnapshot() *snapshot {
	for {
		snap := bogn.currsnapshot()
		if snap == nil {
			return nil
		}
		snap.refer()
		if snap.istrypurge() {
			return snap
		}
		snap.release()
		runtime.Gosched()
	}
	panic("unreachable code")
}

func (bogn *Bogn) mwmetadata(seqno uint64) []byte {
	metadata := map[string]interface{}{
		"seqno":     seqno,
		"flushunix": fmt.Sprintf("%q", time.Now().Unix()),
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	return data
}

func (bogn *Bogn) flushelapsed() bool {
	var md map[string]interface{}
	if snap := bogn.currsnapshot(); snap != nil {
		if _, disk := snap.latestlevel(); disk != nil {
			var metadata []byte
			switch index := disk.(type) {
			case *bubt.Snapshot:
				metadata = index.Metadata()
			default:
				panic("impossible situation")
			}
			if err := json.Unmarshal(metadata, &md); err != nil {
				panic(err)
			}
			x, _ := strconv.Atoi(md["flushunix"].(string))
			if time.Since(time.Unix(int64(x), 0)) > bogn.period {
				return true
			}
		}
	}
	return false
}

func (bogn *Bogn) pickflushdisk(disk0 api.Index) (api.Index, int, int) {
	snap := bogn.currsnapshot()

	if level, disk := snap.latestlevel(); level < 0 && disk0 != nil {
		panic("impossible situation")

	} else if level < 0 {
		return nil, len(snap.disks) - 1, 1

	} else if disk.ID() == disk0.ID() {
		return nil, level - 1, 1

	} else if level0, _, _ := bogn.path2level(disk0.ID()); level0 <= level {
		panic("impossible situation")

	} else {
		_, version, _ := bogn.path2level(disk.ID())
		footprint := float64(disk.(*bubt.Snapshot).Footprint())
		if (float64(snap.memheap()) / footprint) > bogn.ratio {
			return disk, level - 1, 1
		}
		return disk, level, version + 1
	}
	panic("unreachable code")
}

func (bogn *Bogn) pickcompactdisks() (disk0, disk1 api.Index, nextlevel int) {
	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	for i := 0; i < len(disks)-1; i++ {
		disk0, disk1 = disks[i], disks[i+1]
		footprint0 := float64(disk0.(*bubt.Snapshot).Footprint())
		footprint1 := float64(disk1.(*bubt.Snapshot).Footprint())
		if (footprint0 / footprint1) < bogn.ratio {
			continue
		}
		level1, _, _ := bogn.path2level(disk1.ID())
		if nextlevel = snap.nextemptylevel(level1); nextlevel < 0 {
			level0, _, _ := bogn.path2level(disk0.ID())
			if nextlevel = snap.nextemptylevel(level0); nextlevel < 0 {
				nextlevel = level1
			}
		}
		return disk0, disk1, nextlevel
	}
	return nil, nil, -1
}

func (bogn *Bogn) levelname(level, version int, sha string) string {
	return fmt.Sprintf("%v-%v-%v-%v", bogn.name, level, version, sha)
}

func (bogn *Bogn) path2level(filename string) (level, ver int, uuid string) {
	var err error

	parts := strings.Split(filename, "-")
	if len(parts) == 4 && parts[0] == bogn.name {
		if level, err = strconv.Atoi(parts[1]); err != nil {
			return -1, -1, ""
		}
		if ver, err = strconv.Atoi(parts[2]); err != nil {
			return -1, -1, ""
		}
		uuid = parts[3]
	}
	return
}

func (bogn *Bogn) isclosed() bool {
	select {
	case <-bogn.finch:
		return true
	default:
	}
	return false
}

//---- Exported Control methods

func (bogn *Bogn) ID() string {
	return bogn.name
}

func (bogn *Bogn) BeginTxn(id uint64) api.Transactor {
	return nil
}

func (bogn *Bogn) View(id uint64) api.Transactor {
	return nil
}

func (bogn *Bogn) Clone(id uint64) *Bogn {
	return nil
}

func (bogn *Bogn) Stats() map[string]interface{} {
	return nil
}

func (bogn *Bogn) Log() {
	return
}

func (bogn *Bogn) Compact() {
	bogn.compactdisksnaps(bogn.setts)
}

func (bogn *Bogn) Close() {
	close(bogn.finch)
	for atomic.LoadInt64(&bogn.nroutines) > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	for purgesnapshot(bogn.currsnapshot()) {
		time.Sleep(100 * time.Millisecond)
	}
	bogn.setheadsnapshot(nil)
}

func (bogn *Bogn) Destroy() {
	bogn.destroydisksnaps()
}

//---- Exported read methods

func (bogn *Bogn) Get(
	key, value []byte) (v []byte, cas uint64, deleted, ok bool) {

	return bogn.currsnapshot().yget(key, value)
}

func (bogn *Bogn) Scan() api.Iterator {
	return bogn.currsnapshot().iterator()
}

//---- Exported write methods

func (bogn *Bogn) Set(key, value, oldvalue []byte) (ov []byte, cas uint64) {
	bogn.snaprw.RLock()
	ov, cas = bogn.currsnapshot().set(key, value, oldvalue)
	bogn.snaprw.RUnlock()
	return ov, cas
}

func (bogn *Bogn) SetCAS(
	key, value, oldvalue []byte, cas uint64) ([]byte, uint64, error) {

	bogn.snaprw.RLock()
	ov, cas, err := bogn.currsnapshot().setCAS(key, value, oldvalue, cas)
	bogn.snaprw.RUnlock()
	return ov, cas, err
}

func (bogn *Bogn) Delete(key, oldvalue []byte) ([]byte, uint64) {
	bogn.snaprw.RLock()
	lsm := false
	if atomic.LoadInt64(&bogn.dgmstate) == 1 { // auto-enable lsm in dgm
		lsm = true
	}
	ov, cas := bogn.currsnapshot().delete(key, oldvalue, lsm)
	bogn.snaprw.RUnlock()
	return ov, cas
}

//---- local methods

func (bogn *Bogn) newmemstore(level string, seqno uint64) (api.Index, error) {
	name := fmt.Sprintf("%v-%v", bogn.name, level)

	switch bogn.memstore {
	case "llrb":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		index := llrb.NewLLRB(name, llrbsetts)
		index.Setseqno(seqno)
		return index, nil

	case "mvcc":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		index := llrb.NewMVCC(name, llrbsetts)
		index.Setseqno(seqno)
		return index, nil
	}
	return nil, fmt.Errorf("invalid memstore %q", bogn.memstore)
}

func (bogn *Bogn) builddiskstore(
	level, ver int,
	iter api.Iterator) (index api.Index, count uint64, err error) {

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()

	// book-keep largest seqno for this snapshot.
	var diskseqno uint64

	wrap := func(fin bool) ([]byte, []byte, uint64, bool, error) {
		key, val, seqno, deleted, e := iter(fin)
		if seqno > diskseqno {
			diskseqno = seqno
		}
		count++
		return key, val, seqno, deleted, e
	}

	uuid := make([]byte, 64)
	n := lib.Uuid(make([]byte, 8)).Format(uuid)
	name := bogn.levelname(level, ver, string(uuid[:n]))

	bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
	paths := bubtsetts.Strings("diskpaths")
	msize := bubtsetts.Int64("msize")
	zsize := bubtsetts.Int64("zsize")
	bt, err := bubt.NewBubt(name, paths, msize, zsize)
	if err != nil {
		return nil, count, err
	}
	defer bt.Close()

	// build
	if err = bt.Build(wrap, nil); err != nil {
		return nil, count, err
	} else if err = bt.Writemetadata(bogn.mwmetadata(diskseqno)); err != nil {
		return nil, count, err
	}

	// open disk
	mmap := bubtsetts.Bool("mmap")
	ndisk, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		return nil, count, err
	}
	return ndisk, count, nil
}

// open latest versions for each disk level
func (bogn *Bogn) opendisksnaps(setts s.Settings) ([16]api.Index, error) {
	var disks [16]api.Index

	bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
	paths := bubtsetts.Strings("diskpaths")
	mmap := bubtsetts.Bool("mmap")

	for _, path := range paths {
		fis, err := ioutil.ReadDir(path)
		if err != nil {
			log.Errorf("%v %v", bogn.logprefix, err)
			return disks, err
		}
		for _, fi := range fis {
			if level, _, _ := bogn.path2level(fi.Name()); level < 0 {
				continue // not a bogn directory
			}
			disks, err = bogn.buildlevels(fi.Name(), paths, mmap, disks)
			if err != nil {
				return disks, err
			}
		}
	}
	for _, disk := range disks {
		log.Infof("%v open-snapshot %v", bogn.logprefix, disk.ID())
	}
	return disks, nil
}

// compact away older versions in disk levels.
func (bogn *Bogn) compactdisksnaps(setts s.Settings) {
	var disks [16]api.Index

	bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
	paths := bubtsetts.Strings("diskpaths")
	mmap := bubtsetts.Bool("mmap")

	for _, path := range paths {
		fis, err := ioutil.ReadDir(path)
		if err != nil {
			log.Errorf("%v %v", bogn.logprefix, err)
			return
		}
		for _, fi := range fis {
			if level, _, _ := bogn.path2level(fi.Name()); level < 0 {
				continue // not a bogn directory
			}
			disks = bogn.compactlevels(fi.Name(), paths, mmap, disks)
		}
	}
	bogn.closelevels(disks[:]...)
	return
}

func (bogn *Bogn) destroydisksnaps(setts s.Settings) error {
	bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
	paths := bubtsetts.Strings("diskpaths")

	for _, path := range paths {
		fis, err := ioutil.ReadDir(path)
		if err != nil {
			log.Errorf("%v %v", bogn.logprefix, err)
			return
		}
		for _, fi := range fis {
			level, version, uuid := bogn.path2level(fi.Name())
			if level < 0 {
				continue // not a bogn directory
			}
			bubt.PurgeSnapshot(name, paths)
		}
	}
	return
}

// open snapshots for latest version in each level.
func (bogn *Bogn) buildlevels(
	filename string, paths []string, mmap bool,
	disks [16]api.Index) ([16]api.Index, error) {

	level, version, uuid := bogn.path2level(filename)
	if level < 0 { // not a bogn snapshot
		return disks, nil
	}

	name := bogn.levelname(level, version, uuid)
	disk, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		return disks, err
	}

	if od := disks[level]; od == nil { // first version
		disks[level] = disk

	} else if _, over, _ := bogn.path2level(od.ID()); over < version {
		bogn.closelevels(od)
		disks[level] = disk

	} else {
		bogn.closelevels(disk)
	}
	return disks, nil
}

// destory older versions if multiple versions are detected for a disk level
func (bogn *Bogn) compactlevels(
	filename string, paths []string, mmap bool,
	disks [16]api.Index) [16]api.Index {

	level, version, uuid := bogn.path2level(filename)
	if level < 0 { // not a bogn snapshot
		return disks
	}

	name := bogn.levelname(level, version, uuid)
	disk, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		bubt.PurgeSnapshot(name, paths)
		return disks
	}

	if od := disks[level]; od == nil { // first version
		disks[level] = disk

	} else if _, over, _ := bogn.path2level(od.ID()); over < version {
		bogn.destroylevels(disks[level])
		disks[level] = disk

	} else {
		bogn.destroylevels(disk)
	}
	return disks
}

// release resources held in disk levels.
func (bogn *Bogn) closelevels(indexes ...api.Index) {
	for _, index := range indexes {
		index.Close()
	}
}

// destroy disk footprint in disk levels.
func (bogn *Bogn) destroylevels(indexes ...api.Index) {
	for _, index := range indexes {
		index.Close()
		index.Destroy()
	}
}
