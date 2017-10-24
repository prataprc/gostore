package bogr

import "fmt"

import s "github.com/prataprc/gosettings"
import "github.com/cloudfoundry/gosigar"

func Defaultsettings() s.Settings {
	_, _, free := getsysmem()
	return s.Settings{
		"memstore":          "mvcc",
		"dgm":               false,
		"workingset":        false,
		"ratio":             .25,
		"flushperiod":       100,
		"compacttick":       1,
		"llrb.keycapacity":  free,
		"llrb.valcapacity":  free,
		"llrb.snapshottick": 4,
		"llrb.allocator":    "flist",
		"bubt.diskpaths":    "/opt/bogr/",
		"bubt.msize":        4096,
		"bubt.zsize":        4096,
		"bubt.mmap":         true,
	}
}

func getsysmem() (total, used, free uint64) {
	mem := sigar.Mem{}
	mem.Get()
	return mem.Total, mem.Used, mem.Free
}