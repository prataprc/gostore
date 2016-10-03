package api

import "bytes"
import "fmt"
import "reflect"
import "unsafe"

var _ = fmt.Sprintf("dummy")

func Binarycmp(key, limit []byte, partial bool) int {
	if ln := len(limit); partial && ln < len(key) {
		return bytes.Compare(key[:ln], limit[:ln])
	}
	return bytes.Compare(key, limit)
}

func Fixbuffer(buffer []byte, size int64) []byte {
	if size == 0 {
		return buffer
	} else if buffer == nil || int64(cap(buffer)) < size {
		return make([]byte, size)
	}
	return buffer[:size]
}

// Bytes2str morph byte slice to a string without copying. Note that the
// source byte-slice should remain in scope as long as string is in scope.
func Bytes2str(bytes []byte) string {
	if bytes == nil {
		return ""
	}
	sl := (*reflect.SliceHeader)(unsafe.Pointer(&bytes))
	st := &reflect.StringHeader{Data: sl.Data, Len: sl.Len}
	return *(*string)(unsafe.Pointer(st))
}
