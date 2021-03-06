package indextree

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"sync"

	dbm "github.com/tendermint/tm-db"

	"github.com/coinexchain/onvakv/indextree/b"
	"github.com/coinexchain/onvakv/types"
)

const (
	MaxKeyLength = 8192
)

type Iterator = types.Iterator
var _ Iterator = (*ForwardIterMem)(nil)
var _ Iterator = (*BackwardIterMem)(nil)

type ForwardIterMem struct {
	enumerator *b.Enumerator
	tree       *NVTreeMem
	start      []byte
	end        []byte
	key        []byte
	value      uint64
	err        error
}
type BackwardIterMem struct {
	enumerator *b.Enumerator
	tree       *NVTreeMem
	start      []byte
	end        []byte
	key        []byte
	value      uint64
	err        error
}

func (iter *ForwardIterMem) Domain() ([]byte, []byte) {
	return iter.start, iter.end
}
func (iter *ForwardIterMem) Valid() bool {
	return iter.err == nil
}
func (iter *ForwardIterMem) Next() {
	if iter.tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
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
	iter.tree.mtx.RUnlock() // It was locked when iter was created
	if iter.enumerator != nil {
		iter.enumerator.Close()
	}
}

func (iter *BackwardIterMem) Domain() ([]byte, []byte) {
	return iter.start, iter.end
}
func (iter *BackwardIterMem) Valid() bool {
	return iter.err == nil
}
func (iter *BackwardIterMem) Next() {
	if iter.tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
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
	iter.tree.mtx.RUnlock() // It was locked when iter was created
	if iter.enumerator != nil {
		iter.enumerator.Close()
	}
}

/* ============================
Here we implement IndexTree with an in-memory B-Tree and a on-disk RocksDB.
The B-Tree contains only the latest key-position records, while the RocksDB
contains several versions of positions for each key. The keys in RocksDB have
two parts: the original key and 64-bit height. The height means the key-position
record expires (get invalid) at this height. When the height is math.MaxUint64,
the key-position record is up-to-date, i.e., not expired.
*/

type NVTreeMem struct {
	mtx        sync.RWMutex
	bt         *b.Tree
	isWriting  bool
	rocksdb    *RocksDB
	batch      dbm.Batch
	currHeight [8]byte
}

var _ types.IndexTree = (*NVTreeMem)(nil)

func NewNVTreeMem(rocksdb *RocksDB) *NVTreeMem {
	btree := b.TreeNew(bytes.Compare)
	return &NVTreeMem{
		bt:      btree,
		rocksdb: rocksdb,
	}
}

func (tree *NVTreeMem) Close() {
	tree.bt.Close()
}

func (tree *NVTreeMem) ActiveCount() int {
	return tree.bt.Len()
}

// Load the RocksDB and use its up-to-date records to initialize the in-memory B-Tree.
// RocksDB's historical records are ignored.
func (tree *NVTreeMem) Init(repFn func([]byte)) (err error) {
	iter := tree.rocksdb.Iterator([]byte{}, []byte(nil))
	defer iter.Close()
	for iter.Valid() {
		k := iter.Key()
		v := iter.Value()
		if k[0] != 0 {
			continue // the first byte must be zero
		}
		k = k[1:]
		repFn(k) // to report the progress
		if len(k) < 8 {
			panic("key length is too short")
		}
		if len(v) != 8 && len(v) != 0 {
			panic("value length is not 8 or 0")
		}
		if bytes.Equal(k[len(k)-8:], []byte{255, 255, 255, 255, 255, 255, 255, 255}) {
			//write the up-to-date value
			tree.bt.Set(k[:len(k)-8], binary.LittleEndian.Uint64(v))
		}
		iter.Next()
	}
	return nil
}

// Begin the write phase of block execution
func (tree *NVTreeMem) BeginWrite(currHeight int64) {
	tree.mtx.Lock()
	if tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
	tree.isWriting = true
	binary.BigEndian.PutUint64(tree.currHeight[:], uint64(currHeight))
}

// End the write phase of block execution
func (tree *NVTreeMem) EndWrite() {
	if !tree.isWriting {
		panic("tree.isWriting cannot be false! bug here...")
	}
	tree.isWriting = false
	tree.mtx.Unlock()
}

// Update or insert a key-position record to B-Tree and RocksDB
// Write the historical record to RocksDB
func (tree *NVTreeMem) Set(k []byte, v uint64) {
	if !tree.isWriting {
		panic("tree.isWriting must be true! bug here...")
	}
	oldV, oldVExists := tree.bt.PutNewAndGetOld(k, v)

	if tree.rocksdb == nil {
		return
	}
	newK := make([]byte, 0, 1+len(k)+8)
	newK = append(newK, byte(0)) //first byte is always zero
	newK = append(newK, k...)
	newK = append(newK, tree.currHeight[:]...)
	var buf [8]byte
	if oldVExists {
		binary.LittleEndian.PutUint64(buf[:], oldV)
		tree.batchSet(newK, buf[:]) // write a historical value
	} else {
		tree.batchSet(newK, []byte{}) // write a historical empty value
	}

	binary.LittleEndian.PutUint64(buf[:], v)
	binary.BigEndian.PutUint64(newK[len(newK)-8:], math.MaxUint64)
	tree.batchSet(newK, buf[:]) // write the up-to-date value
}

func (tree *NVTreeMem) batchSet(key, value []byte) {
	tree.rocksdb.CurrBatch().Set(key, value)
}

func (tree *NVTreeMem) batchDelete(key []byte) {
	tree.rocksdb.CurrBatch().Delete(key)
}

// Get the up-to-date position of k, from the B-Tree
func (tree *NVTreeMem) Get(k []byte) (uint64, bool) {
	if tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
	tree.mtx.RLock()
	defer tree.mtx.RUnlock()
	return tree.bt.Get(k)
}

// Get the position of k, at the specified height.
func (tree *NVTreeMem) GetAtHeight(k []byte, height uint64) (position uint64, ok bool) {
	if h, enable := tree.rocksdb.GetPruneHeight(); enable && height <= h {
		return 0, false
	}
	newK := make([]byte, 1+len(k)+8) // all bytes equal zero
	copy(newK[1:], k)
	binary.BigEndian.PutUint64(newK[1+len(k):], height+1)
	iter := tree.rocksdb.Iterator(newK, nil)
	defer iter.Close()
	if !iter.Valid() {
		return 0, false
	}

	binary.BigEndian.PutUint64(newK[len(k):], math.MaxUint64)
	if bytes.Compare(iter.Key(), newK) > 0 { // to a different k
		return 0, false
	}

	v := iter.Value()
	if len(v) == 0 {
		ok = false
	} else {
		ok = true
		position = binary.LittleEndian.Uint64(v)
	}
	return
}

// Delete a key-position record in B-Tree and RocksDB
// Write the historical record to RocksDB
func (tree *NVTreeMem) Delete(k []byte) {
	if !tree.isWriting {
		panic("tree.isWriting must be true! bug here...")
	}
	oldV, ok := tree.bt.Get(k)
	if !ok {
		panic("deleting a nonexistent key! bug here...")
	}
	tree.bt.Delete(k)

	if tree.rocksdb == nil {
		return
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], oldV)
	newK := make([]byte, 0, 1+len(k)+8)
	newK = append(newK, byte(0)) //first byte is always zero
	newK = append(newK, k...)
	newK = append(newK, tree.currHeight[:]...)
	tree.batchSet(newK, buf[:]) // write a historical value

	binary.BigEndian.PutUint64(newK[len(newK)-8:], math.MaxUint64)
	tree.batchDelete(newK) // delete the up-to-date value
}

// Create a forward iterator from the B-Tree
func (tree *NVTreeMem) Iterator(start, end []byte) Iterator {
	if tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
	tree.mtx.RLock()
	iter := &ForwardIterMem{tree: tree, start: start, end: end}
	if bytes.Compare(start, end) >= 0 {
		iter.err = io.EOF
		return iter
	}
	iter.enumerator, _ = tree.bt.Seek(start)
	iter.Next() //fill key, value, err
	return iter
}

// Create a backward iterator from the B-Tree
func (tree *NVTreeMem) ReverseIterator(start, end []byte) Iterator {
	if tree.isWriting {
		panic("tree.isWriting cannot be true! bug here...")
	}
	tree.mtx.RLock()
	iter := &BackwardIterMem{tree: tree, start: start, end: end}
	if bytes.Compare(start, end) >= 0 {
		iter.err = io.EOF
		return iter
	}
	var ok bool
	iter.enumerator, ok = tree.bt.Seek(end)
	if ok { // [start, end) end is exclusive
		iter.enumerator.Prev()
	}
	iter.Next() //fill key, value, err
	return iter
}

