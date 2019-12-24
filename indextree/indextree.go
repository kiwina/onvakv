package indextree

import (
	"bytes"
	"io"
	"math"
	"sync"
	"encoding/binary"

	"github.com/coinexchain/onvakv/indextree/b"
	"github.com/coinexchain/onvakv/types"
)

const (
	MaxKeyLength = 8192
)

type Iterator = types.Iterator


// ============================
// Here we implement IndexTree with an in-memory B-Tree and a file log

type ForwardIterMem struct {
	enumerator *b.Enumerator
	start      []byte
	end        []byte
	key        []byte
	value      uint64
	err        error
}
type BackwardIterMem struct {
	enumerator *b.Enumerator
	start      []byte
	end        []byte
	key        []byte
	value      uint64
	err        error
}
var _ Iterator = (*ForwardIterMem)(nil)
var _ Iterator = (*BackwardIterMem)(nil)

func (iter *ForwardIterMem) Domain() ([]byte, []byte) {
	return iter.start, iter.end
}
func (iter *ForwardIterMem) Valid() bool {
	return iter.err == nil
}
func (iter *ForwardIterMem) Next() {
	if iter.err == nil {
		iter.key, iter.value, iter.err = iter.enumerator.Next()
		if bytes.Compare(iter.key, iter.end) >= 0 {
			iter.err = io.EOF
		}
	}
}
func (iter *ForwardIterMem) Key() []byte {
	return iter.key
}
func (iter *ForwardIterMem) Value() uint64 {
	return iter.value
}
func (iter *ForwardIterMem) Close() {
	iter.enumerator.Close()
}

func (iter *BackwardIterMem) Domain() ([]byte, []byte) {
	return iter.start, iter.end
}
func (iter *BackwardIterMem) Valid() bool {
	return iter.err == nil
}
func (iter *BackwardIterMem) Next() {
	if iter.err == nil {
		iter.key, iter.value, iter.err = iter.enumerator.Prev()
		if bytes.Compare(iter.key, iter.start) < 0 {
			iter.err = io.EOF
		}
	}
}
func (iter *BackwardIterMem) Key() []byte {
	return iter.key
}
func (iter *BackwardIterMem) Value() uint64 {
	return iter.value
}
func (iter *BackwardIterMem) Close() {
	iter.enumerator.Close()
}

//------------

type NVTreeMem struct {
	mtx        sync.RWMutex
	bt         *b.Tree
	isWriting  bool
	rocksdb    *RocksDB
	batch      *rocksDBBatch
	currHeight [8]byte
}
var _ types.IndexTree = (*NVTreeMem)(nil)

func NewNVTreeMem(entryCountLimit int) *NVTreeMem {
	btree := b.TreeNew(bytes.Compare)
	return &NVTreeMem {
		bt:               btree,
	}
}

func (tree *NVTreeMem) Init(dirname string, repFn func(string)) (err error) {
	tree.rocksdb, err = NewRocksDB("itree", dirname)
	if err != nil {
		return err
	}
	iter := tree.rocksdb.Iterator([]byte{}, []byte(nil))
	defer iter.Close()
	for iter.Valid() {
		k := iter.Key()
		v := iter.Value()
		if len(k) < 8 {
			panic("key length is too short")
		}
		if len(v) != 8 && len(v) != 0 {
			panic("value length is not 8 or 0")
		}
		if bytes.Equal(k[len(k)-8:], []byte{255,255,255,255,255,255,255,255}) {
			//write the up-to-date value
			tree.bt.Set(k[:len(k)-8], binary.BigEndian.Uint64(v))
		}
		iter.Next()
	}
	return nil
}

func (tree *NVTreeMem) BeginWrite(currHeight int64) {
	tree.mtx.Lock()
	if tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
	tree.isWriting = true
	tree.batch = tree.rocksdb.NewBatch()
	binary.BigEndian.PutUint64(tree.currHeight[:], uint64(currHeight))
}

func (tree *NVTreeMem) EndWrite() {
	if !tree.isWriting {
		panic("tree.isWriting cannot be false! bug here...")
	}
	tree.isWriting = false
	tree.batch.WriteSync()
	tree.batch.Close()
	tree.mtx.Unlock()
}

func (tree *NVTreeMem) Set(k []byte, v uint64) {
	if !tree.isWriting {
		panic("tree.isWriting must be true! bug here...")
	}
	oldVExists := false
	oldV, oldVExists := tree.bt.PutNewAndGetOld(k, v)

	newK := make([]byte, 0, len(k)+8)
	newK = append(newK, k...)
	newK = append(newK, tree.currHeight[:]...)
	var buf [8]byte
	if oldVExists {
		binary.BigEndian.PutUint64(buf[:], oldV)
		tree.batch.Set(newK, buf[:]) // write a historical value
	} else {
		tree.batch.Set(newK, []byte{}) // write a historical empty value
	}

	binary.BigEndian.PutUint64(buf[:], v)
	binary.BigEndian.PutUint64(newK[len(newK)-8:], math.MaxUint64)
	tree.batch.Set(newK, buf[:]) // write the up-to-date value
}

func (tree *NVTreeMem) Get(k []byte) (uint64, bool) {
	if tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
	tree.mtx.RLock()
	defer tree.mtx.RUnlock()
	return tree.bt.Get(k)
}

func (tree *NVTreeMem) GetAtHeight(k []byte, height uint64) (res uint64, ok bool) {
	if height <= tree.rocksdb.GetPruneHeight() {
		return 0, false
	}
	newK := make([]byte, len(k)+8)
	copy(newK, k)
	binary.BigEndian.PutUint64(newK[len(k):], height+1)
	iter := tree.rocksdb.Iterator(newK, nil)
	defer iter.Close()
	if !iter.Valid() {
		return 0, false
	}

	binary.BigEndian.PutUint64(newK[len(k):], math.MaxUint64)
	if bytes.Compare(iter.Key(), newK) > 0 {
		return 0, false
	}

	v := iter.Value()
	if len(v) == 0 {
		ok = false
	} else {
		ok = true
		res = binary.BigEndian.Uint64(v)
	}
	return
}

func (tree *NVTreeMem) Delete(k []byte) {
	if !tree.isWriting {
		panic("tree.isWriting must be true! bug here...")
	}
	oldV, ok := tree.bt.Get(k)
	if !ok {
		panic("deleting a nonexistent key! bug here...")
	}
	tree.bt.Delete(k)

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], oldV)
	newK := make([]byte, 0, len(k)+8)
	newK = append(newK, k...)
	newK = append(newK, tree.currHeight[:]...)
	tree.batch.Set(newK, buf[:]) // write a historical value

	binary.BigEndian.PutUint64(newK[len(newK)-8:], math.MaxUint64)
	tree.batch.Delete(newK) // delete the up-to-date value
}

func (tree *NVTreeMem) Iterator(start, end []byte) Iterator {
	iter := &ForwardIterMem{start:start, end:end}
	if bytes.Compare(start, end) >= 0 {
		iter.err = io.EOF
		return iter
	}
	iter.enumerator, _ = tree.bt.Seek(start)
	iter.Next() //fill key, value, err
	return iter
}

func (tree *NVTreeMem) ReverseIterator(start, end []byte) Iterator {
	iter := &BackwardIterMem{start:start, end:end}
	if bytes.Compare(start, end) >= 0 {
		iter.err = io.EOF
		return iter
	}
	iter.enumerator, _ = tree.bt.Seek(end)
	//now iter.enumerator >= k, we want end is exclusive
	iter.enumerator.Prev()
	iter.Next() //fill key, value, err
	return iter
}

func (tree *NVTreeMem) SetPruneHeight(h uint64) {
	tree.rocksdb.SetPruneHeight(h)
}

