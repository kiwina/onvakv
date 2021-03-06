package fuzz

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"math/rand"
	"sync"
	"strconv"

	"github.com/coinexchain/randsrc"
	"github.com/coinexchain/onvakv"
	"github.com/coinexchain/onvakv/store"
	"github.com/coinexchain/onvakv/store/rabbit"
	storetypes "github.com/coinexchain/onvakv/store/types"
)

const (
	FirstByteOfCacheableKey = byte(15)
)

var (
	GuardStart = []byte{0, 0, 0, 0, 0, 0, 0, 0}
	GuardEnd = []byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255}
	EndKey = []byte{255, 255, 255, 255, 255, 255, 255, 255, 255}
)

var DBG bool

func runTest(cfg *FuzzConfig) {
	DBG = false
	randFilename := os.Getenv("RANDFILE")
	if len(randFilename) == 0 {
		fmt.Printf("No RANDFILE specified. Exiting...")
		return
	}
	roundCount, err := strconv.Atoi(os.Getenv("RANDCOUNT"))
	if err != nil {
		panic(err)
	}

	rs := randsrc.NewRandSrcFromFileWithSeed(randFilename, []byte{0})
	var root storetypes.RootStoreI
	if cfg.RootType == "MockRoot" {
		root = store.NewMockRootStore()
	} else if cfg.RootType == "MockDataTree" {
		os.RemoveAll("./rocksdb.db")
		okv := onvakv.NewOnvaKV4Mock([][]byte{GuardStart, GuardEnd})
		root = store.NewRootStore(okv, nil, func(k []byte) bool {
			return (k[0]&FirstByteOfCacheableKey) == FirstByteOfCacheableKey
		})
	} else if cfg.RootType == "Real" {
		os.RemoveAll("./onvakv4test")
		okv, err := onvakv.NewOnvaKV("./onvakv4test", false, [][]byte{GuardStart, GuardEnd})
		if err != nil {
			panic(err)
		}
		root = store.NewRootStore(okv, nil, func(k []byte) bool {
			return (k[0]&FirstByteOfCacheableKey) == FirstByteOfCacheableKey
		})
	} else {
		panic("Invalid RootType "+cfg.RootType)
	}
	ref := NewRefStore()
	fmt.Printf("Initialized\n")

	for i := 0; i< roundCount; i++ {
		//if i > 66 {DBG = true}
		fmt.Printf("Block %d\n", i)
		root.CheckConsistency()
		block := GenerateRandBlock(i, ref, rs, cfg)
		//ExecuteBlock(i, root, &block, rs, cfg, false) //not in parrallel
		ExecuteBlock(i, root, &block, rs, cfg, true) //in parrallel
	}
	root.Close()
	if cfg.RootType == "MockDataTree" {
		os.RemoveAll("./rocksdb.db")
	} else if cfg.RootType == "Real" {
		os.RemoveAll("./onvakv4test")
	}
}

const (
	OpRead = 8
	OpIterate = 6
	OpWrite = 1
	OpDelete = 0
)

type Coord struct {
	x, y uint32
}
func (coord *Coord) ToBytes() []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint32(buf[:4], coord.x)
	binary.LittleEndian.PutUint32(buf[4:], coord.y)
	return buf[:]
}

func (coord *Coord) FromBytes(buf []byte) {
	if len(buf) != 8 {
		panic(fmt.Sprintf("length is not 8: %#v", buf))
	}
	coord.x = binary.LittleEndian.Uint32(buf[:4])
	coord.y = binary.LittleEndian.Uint32(buf[4:])
}

func (coord *Coord) DeepCopy() interface{} {
	return &Coord{
		x: coord.x,
		y: coord.y,
	}
}

type FuzzConfig struct {
	MaxReadCountInTx     uint32
	MaxIterCountInTx     uint32
	MaxWriteCountInTx    uint32
	MaxDeleteCountInTx   uint32
	MaxTxCountInEpoch    uint32
	MaxEpochCountInBlock uint32
	EffectiveBits        uint32
	MaxIterDistance      uint32
	MaxActiveCount       int
	TxSucceedRatio       float32
	BlockSucceedRatio    float32
	DelAfterIterRatio    float32
	RootType             string //MockRoot MockDataTree Real
	TestRabbit           bool
}

type Pair struct {
	Key, Value []byte
}

type Operation struct {
	opType  int
	key     [8]byte
	keyEnd  [8]byte
	value   []byte
	results []Pair
	randNum uint32
}

type Tx struct {
	OpList  []Operation
	Succeed bool
}

type Epoch struct {
	TxList []*Tx
}

type Block struct {
	EpochList []Epoch
	Succeed bool
}

func getRand8Bytes(rs randsrc.RandSrc, cfg *FuzzConfig, touchedKeys map[uint64]struct{}) (res [8]byte) {
	sh := 62 - cfg.EffectiveBits
	if touchedKeys == nil {
		i := rs.GetUint64()
		i = ((i<<sh)>>sh)|3
		binary.LittleEndian.PutUint64(res[:], i)
		return
	}
	for {
		i := rs.GetUint64()
		i = ((i<<sh)>>sh)|3
		if _, ok := touchedKeys[i]; ok {
			continue
		} else {
			binary.LittleEndian.PutUint64(res[:], i)
			break
		}
	}
	return
}

func UpdateRefStoreWithTx(ref *RefStore, tx *Tx) {
	for _, op := range tx.OpList {
		if op.opType == OpWrite {
			ref.Set(op.key[:], op.value[:])
		}
		if op.opType == OpDelete {
			ref.Delete(op.key[:])
		}
	}
}

func RecheckIter(ref *RefStore, rs randsrc.RandSrc, cfg *FuzzConfig, tx *Tx) {
	for i, op := range tx.OpList {
		if op.opType != OpIterate {
			continue
		}
		var iter storetypes.ObjIterator
		if bytes.Compare(op.key[:], op.keyEnd[:]) < 0 {
			iter = ref.Iterator(op.key[:], op.keyEnd[:])
		} else {
			iter = ref.ReverseIterator(op.keyEnd[:], op.key[:])
		}

		iterOK := true
		for _, pair := range op.results {
			if !iter.Valid() {
				iterOK = false
				break
			}
			if !bytes.Equal(iter.Key(), pair.Key) {
				iterOK = false
				break
			}
			if !bytes.Equal(iter.Value(), pair.Value) {
				iterOK = false
				break
			}
			iter.Next()
		}
		if iter.Valid() && len(op.results) < int(cfg.MaxIterDistance) {
			iterOK = false
		}
		if !iterOK {
			tx.OpList[i].value = nil //nil-value marks it as invalid
		}

		iter.Close()
	}
}

func GenerateRandTx(ref *RefStore, rs randsrc.RandSrc, cfg *FuzzConfig, touchedKeys map[uint64]struct{}) *Tx {
	readCount, iterCount, writeCount, deleteCount := uint32(0), uint32(0), uint32(0), uint32(0)
	maxReadCount := rs.GetUint32()%(cfg.MaxReadCountInTx+1)
	maxIterCount := rs.GetUint32()%(cfg.MaxIterCountInTx+1)
	maxWriteCount := rs.GetUint32()%(cfg.MaxWriteCountInTx+1)
	if cfg.MaxActiveCount > 0 && ref.Size() > cfg.MaxActiveCount {
		maxWriteCount = 0
	}
	maxDeleteCount := rs.GetUint32()%(cfg.MaxDeleteCountInTx+1)
	tx := Tx{
		OpList: make([]Operation, 0, maxReadCount+maxWriteCount+maxDeleteCount),
		Succeed: float32(rs.GetUint32()%0x10000)/float32(0x10000) < cfg.TxSucceedRatio,
	}
	var undoList []UndoOp
	if !tx.Succeed {
		undoList = make([]UndoOp, 0, maxWriteCount + maxDeleteCount)
	}
	for readCount!=maxReadCount || iterCount!=maxIterCount || writeCount!=maxWriteCount || deleteCount!=maxDeleteCount {
		if rs.GetUint32()%4 == 0 && readCount < maxReadCount {
			key := getRand8Bytes(rs, cfg, touchedKeys)
			tx.OpList = append(tx.OpList, Operation{
				opType:  OpRead,
				key:     key,
				value:   ref.Get(key[:]),
				randNum: rs.GetUint32(),
			})
			readCount++
		}
		if rs.GetUint32()%4 == 0 && iterCount < maxIterCount {
			op := Operation{
				opType:  OpIterate,
				key:     getRand8Bytes(rs, cfg, nil),
				keyEnd:  getRand8Bytes(rs, cfg, nil),
				value:   []byte{1}, //make its non-nil, which marks this op as valid
				randNum: rs.GetUint32(),
			}
			var iter storetypes.ObjIterator
			if bytes.Compare(op.key[:], op.keyEnd[:]) < 0 {
				iter = ref.Iterator(op.key[:], op.keyEnd[:])
			} else {
				iter = ref.ReverseIterator(op.keyEnd[:], op.key[:])
			}
			iterSucceed := true
			if iter.Valid() {
				for len(op.results) < int(cfg.MaxIterDistance) {
					if !iter.Valid() {break}
					changed := ref.IsChangedInSameEpoch(iter.Key())
					if DBG {fmt.Printf("IsChangedInSameEpoch %v %#v\n", changed, iter.Key())}
					if changed {
						iterSucceed = false
						break
					}
					if iter.Value() == nil {
						if DBG {fmt.Printf("skipping a pair deleted in same transaction %#v\n", iter.Key())}
						iter.Next()
						continue
					}
					op.results = append(op.results, Pair{
						Key:   append([]byte{}, iter.Key()...),
						Value: append([]byte{}, iter.Value()...),
					})
					iter.Next()
				}
			}
			if iterSucceed {
				tx.OpList = append(tx.OpList, op)
			}
			iter.Close()
			iterCount++
		}
		if rs.GetUint32()%4 == 0 && writeCount < maxWriteCount {
			v := getRand8Bytes(rs, cfg, nil)
			op := Operation{
				opType:  OpWrite,
				key:     getRand8Bytes(rs, cfg, touchedKeys),
				value:   v[:],
				randNum: rs.GetUint32(),
			}
			undo := ref.Set(op.key[:], op.value[:])
			if tx.Succeed {
				if DBG {fmt.Printf("MarkSet %#v\n", op.key[:])}
				ref.MarkSet(op.key[:])
			} else {
				undoList = append(undoList, undo)
			}
			tx.OpList = append(tx.OpList, op)
			writeCount++
		}
		if rs.GetUint32()%4 == 0 && deleteCount < maxDeleteCount {
			delIter := tx.Succeed && float32(rs.GetUint32()%0x10000)/float32(0x10000) < cfg.DelAfterIterRatio
			if delIter {
				key := getRand8Bytes(rs, cfg, nil)
				keyEnd := getRand8Bytes(rs, cfg, nil)
				var iter storetypes.ObjIterator
				if bytes.Compare(key[:], keyEnd[:]) < 0 {
					iter = ref.Iterator(key[:], keyEnd[:])
				} else {
					iter = ref.ReverseIterator(keyEnd[:], key[:])
				}
				for i := uint32(0); i < cfg.MaxIterDistance && iter.Valid(); i++ {
					_, ok := touchedKeys[binary.LittleEndian.Uint64(iter.Key())]
					if ok {
						break
					}
					op := Operation{opType: OpDelete}
					op.randNum = rs.GetUint32()
					copy(op.key[:], iter.Key())
					ref.Delete(op.key[:])
					ref.MarkDelete(op.key[:])
					if DBG {fmt.Printf("MarkDelete %#v\n", op.key[:])}
					tx.OpList = append(tx.OpList, op)
					iter.Next()
				}
				iter.Close()
			} else {
				op := Operation{
					opType:  OpDelete,
					key:     getRand8Bytes(rs, cfg, touchedKeys),
					randNum: rs.GetUint32(),
				}
				undo := ref.Delete(op.key[:])
				if tx.Succeed {
					ref.MarkDelete(op.key[:])
					if DBG {fmt.Printf("MarkDelete %#v\n", op.key[:])}
				} else {
					undoList = append(undoList, undo)
				}
				tx.OpList = append(tx.OpList, op)
			}
			deleteCount++
		}
	}
	if tx.Succeed { // to prevent inter-tx dependency
		for _, op := range tx.OpList {
			if op.opType == OpRead || op.opType == OpWrite || op.opType == OpDelete {
				touchedKeys[binary.LittleEndian.Uint64(op.key[:])] = struct{}{}
			}
		}
	} else { // to recovery old state
		for i := len(undoList)-1; i >= 0; i-- {
			undoOp := undoList[i]
			if undoOp.oldStatus == storetypes.Missed {
				ref.RealDelete(undoOp.key)
			} else if undoOp.oldStatus == storetypes.JustDeleted {
				ref.Delete(undoOp.key)
			} else {
				ref.RealSet(undoOp.key, undoOp.value)
			}
		}
	}
	return &tx
}

func GenerateRandEpoch(height, epochNum int, ref *RefStore, rs randsrc.RandSrc, cfg *FuzzConfig, blkSuc bool) Epoch {
	keyCountEstimated := cfg.MaxTxCountInEpoch*(cfg.MaxReadCountInTx+cfg.MaxIterCountInTx*cfg.MaxIterDistance*2+
		cfg.MaxWriteCountInTx+cfg.MaxDeleteCountInTx)/2
	touchedKeys := make(map[uint64]struct{}, keyCountEstimated)
	txCount := rs.GetUint32()%(cfg.MaxTxCountInEpoch+1)
	epoch := Epoch{TxList: make([]*Tx, int(txCount))}
	for i := range epoch.TxList {
		tx := GenerateRandTx(ref, rs, cfg, touchedKeys)
		if DBG {
			fmt.Printf("FinishGeneration h:%d (%v) epoch %d tx %d (%v) of %d\n", height, blkSuc, epochNum, i, tx.Succeed, txCount)
			for j, op := range tx.OpList {
				fmt.Printf("See operation %d of %d\n", j, len(tx.OpList))
				fmt.Printf("%#v\n", op)
			}
		}
		epoch.TxList[i] = tx
	}
	ref.SwitchEpoch()
	for _, tx := range epoch.TxList {
		RecheckIter(ref, rs, cfg, tx)
	}

	iter := ref.Iterator([]byte{}, EndKey)
	defer iter.Close()
	for iter.Valid() {
		if DBG {fmt.Printf("GEN.AT %d-%d key: %#v Value:%#v\n", height, epochNum, iter.Key(), iter.Value())}
		iter.Next()
	}

	return epoch
}

func GenerateRandBlock(height int, ref *RefStore, rs randsrc.RandSrc, cfg *FuzzConfig) Block {
	epochCount := rs.GetUint32()%(cfg.MaxEpochCountInBlock+1)
	block := Block{EpochList: make([]Epoch, epochCount)}
	block.Succeed = float32(rs.GetUint32()%0x10000)/float32(0x10000) < cfg.BlockSucceedRatio
	if !block.Succeed {
		ref = ref.Clone()
	}
	for i := range block.EpochList {
		if DBG {fmt.Printf("Generating h:%d epoch %d of %d\n", height, i, epochCount)}
		block.EpochList[i] = GenerateRandEpoch(height, i, ref, rs, cfg, block.Succeed)
	}
	return block
}

func MyGet(multi storetypes.MultiStoreI, randNum uint32, key []byte) []byte {
	res := MyGetHelper(multi, randNum, key)
	if multi.Has(key) != (len(res) > 0) {
		panic("Bug in Has")
	}
	return res
}

func MyGetHelper(multi storetypes.MultiStoreI, randNum uint32, key []byte) []byte {
	var coord Coord
	var ptr storetypes.Serializable
	ptr = &coord

	switch randNum%3 {
	case 1:
		return multi.Get(key)
	case 2:
		multi.GetReadOnlyObj(key, &ptr)
		if ptr == nil {
			return nil
		}
		return ptr.ToBytes()
	default:
		multi.GetObj(key, &ptr)
		if ptr == nil {
			return nil
		}
		res := ptr.ToBytes()
		multi.SetObj(key, ptr)
		return res
	}
}

func MySet(multi storetypes.MultiStoreI, randNum uint32, key, value []byte) {
	var coord Coord
	var ptr storetypes.Serializable
	ptr = &coord
	if randNum%2 == 0 {
		ptr.FromBytes(value)
		multi.SetObj(key, ptr)
	} else {
		multi.Set(key, value)
	}
}

func MyIterValue(randNum int32, iter storetypes.ObjIterator) []byte {
	var coord Coord
	var ptr storetypes.Serializable
	ptr = &coord
	if randNum%2 == 0 {
		iter.ObjValue(&ptr)
		if ptr == nil {
			return nil
		}
		return ptr.ToBytes()
	} else {
		return iter.Value()
	}
}

func CheckTx(height, epochNum, txNum int, multi storetypes.MultiStoreI, tx *Tx, rs randsrc.RandSrc, cfg *FuzzConfig, blkSuc bool) {
	for i, op := range tx.OpList {
		if DBG {
			fmt.Printf("Check %d-%d (%v) tx %d (%v) operation %d of %d\n", height, epochNum, blkSuc, txNum,  tx.Succeed, i, len(tx.OpList))
			fmt.Printf("%#v\n", op)
		}
		if op.opType == OpRead {
			bz := MyGet(multi, op.randNum, op.key[:])
			if !bytes.Equal(op.value[:], bz) {
				panic(fmt.Sprintf("Error in Get %#v real %#v expected %#v", op.key[:], bz, op.value[:]))
			}
		}
		if op.opType == OpIterate && len(op.value) != 0 {
			var iter storetypes.ObjIterator
			if bytes.Compare(op.key[:], op.keyEnd[:]) < 0 {
				iter = multi.Iterator(op.key[:], op.keyEnd[:])
			} else {
				iter = multi.ReverseIterator(op.keyEnd[:], op.key[:])
			}
			panicReason := ""
			rand := rand.New(rand.NewSource(int64(op.randNum)))
			for _, pair := range op.results {
				if !iter.Valid() {
					panicReason = "Iterator Invalid"
					break
				}
				if !bytes.Equal(iter.Key(), pair.Key) {
					panicReason = fmt.Sprintf("Key mismatch real %#v expect %#v", iter.Key(), pair.Key)
					break
				}
				if !bytes.Equal(MyIterValue(rand.Int31(), iter), pair.Value) {
					panicReason = fmt.Sprintf("Value mismatch real %#v expect %#v", iter.Value(), pair.Value)
					break
				}
				//if DBG {fmt.Printf("Key match real %#v expect %#v\n", iter.Key(), pair.Key)}
				iter.Next()
			}
			if len(panicReason) == 0 && iter.Valid() && len(op.results) < int(cfg.MaxIterDistance) {
				panicReason = "Iterator Should be Invalid"
				if MyIterValue(rand.Int31(), iter) != nil {
					panicReason = "Iterator Should be Invalid and Value should be nil"
				}
			}
			if len(panicReason) != 0 {
				fmt.Printf("Remaining (at most 10):\n")
				for i := 0; i < 10 && iter.Valid(); i++ {
					fmt.Printf("key: %#v  value: %#v\n", iter.Key(), iter.Value())
					iter.Next()
				}
				panic(panicReason)
			}
			iter.Close()
		}
		if op.opType == OpWrite {
			MySet(multi, op.randNum, op.key[:], op.value[:])
		}
		if op.opType == OpDelete {
			multi.Delete(op.key[:])
		}
	}
}

func ExecuteBlock(height int, root storetypes.RootStoreI, block *Block, rs randsrc.RandSrc, cfg *FuzzConfig, inParallel bool) {
	//showTrunk := func(trunk *store.TrunkStore, epochNum, txNum int, blkSuc, txSuc bool) {
	//	fmt.Printf("Dumping\n")
	//	iter := trunk.Iterator([]byte{}, EndKey)
	//	defer iter.Close()
	//	for iter.Valid() {
	//		fmt.Printf("CHECK.AT %d-%d (%v) tx %d (%v) key: %#v Value:%#v\n", height, epochNum, blkSuc, txNum, txSuc, iter.Key(), iter.Value())
	//		iter.Next()
	//	}
	//}
	root.SetHeight(int64(height))
	trunk := root.GetTrunkStore().(*store.TrunkStore)
	for i, epoch := range block.EpochList {
		if DBG {fmt.Printf("Check h:%d (%v) epoch %d of %d\n", height, block.Succeed, i, len(block.EpochList))}
		dbList := make([]storetypes.MultiStoreI, len(epoch.TxList))
		var wg sync.WaitGroup
		for j, tx := range epoch.TxList {
			if cfg.TestRabbit {
				dbList[j] = rabbit.NewRabbitStore(trunk)
			} else {
				dbList[j] = trunk.Cached()
			}
			if DBG {fmt.Printf("Check h:%d (%v) epoch %d tx %d (%v) of %d\n", height, block.Succeed, i, j, tx.Succeed, len(epoch.TxList))}
			if inParallel {
				wg.Add(1)
				go func(tx *Tx, j int) {
					CheckTx(height, i, j, dbList[j], tx, rs, cfg, block.Succeed)
					wg.Done()
				}(tx, j)
			} else {
				CheckTx(height, i, j, dbList[j], tx, rs, cfg, block.Succeed)
			}

		}
		if inParallel {wg.Wait()}
		for j, tx := range epoch.TxList {
			if DBG {fmt.Printf("WriteBack %d-%d tx %d : %v\n", height, i, j, tx.Succeed)}
			dbList[j].Close(tx.Succeed)
			//showTrunk(trunk, i, j, tx.Succeed, block.Succeed)
		}
	}
	trunk.Close(block.Succeed)
	if DBG {
		iter := root.Iterator([]byte{}, EndKey)
		defer iter.Close()
		for iter.Valid() {
			fmt.Printf("BLOCK.AT %d (%v) key: %#v Value:%#v\n", height, block.Succeed, iter.Key(), iter.Value())
			iter.Next()
		}
	}
}

