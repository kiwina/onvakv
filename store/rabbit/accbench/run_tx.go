package main

import (
	"fmt"
	"encoding/binary"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/mmcloughlin/meow"

	"github.com/coinexchain/onvakv"
	"github.com/coinexchain/onvakv/store"
	"github.com/coinexchain/onvakv/store/rabbit"
)

const (
	NumTxInEpoch = 1024
	NumWorkers = 128
	NumTxPerWorker =  NumTxInEpoch / NumWorkers
	NumEpochInBlock = 32
)

type Block struct {
	epochList [NumEpochInBlock]Epoch
}

type Epoch struct {
	jobList [NumWorkers]Job
}

type AccountAndNum struct {
	acc *rabbit.CachedValue
	num [rabbit.KeySize]byte
}

type Job struct {
	txList         [NumTxPerWorker]Tx
	changedAccList []AccountAndNum
}

func ReadEpoch(fin *os.File) (epoch Epoch) {
	hash64 := meow.New64(0)
	for i := 0; i < NumWorkers; i++ {
		for j := 0; j < NumTxPerWorker; j++ {
			var bz [TxLen]byte
			_, err := fin.Read(bz[:])
			if err != nil {
				panic(err)
			}
			tx := ParseTx(bz)
			epoch.jobList[i].txList[j] = tx
			tx.UpdateHash64(hash64)
		}
		epoch.jobList[i].changedAccList = make([]AccountAndNum, 0, 2*NumTxPerWorker)
	}
	var buf [8]byte
	_, err := fin.Read(buf[:])
	if err != nil {
		panic(err)
	}
	if hash64.Sum64() != binary.LittleEndian.Uint64(buf[:]) {
		panic("Epoch checksum error!")
	}
	return
}

func (epoch Epoch) Check() bool {
	touchedNum := make(map[[rabbit.KeySize]byte]struct{}, 2*NumTxInEpoch)
	for i := 0; i < NumWorkers; i++ {
		for j := 0; j < NumTxPerWorker; j++ {
			tx := epoch.jobList[i].txList[j]
			_, fromConflict := touchedNum[tx.FromNum]
			_, toConflict := touchedNum[tx.ToNum]
			if fromConflict || toConflict {
				return false
			}
			touchedNum[tx.FromNum] = struct{}{}
			touchedNum[tx.ToNum] = struct{}{}
		}
	}
	return true
}

func (epoch Epoch) Run(root *store.RootStore) {
	var wg sync.WaitGroup
	isValid := true
	wg.Add(1+len(epoch.jobList))
	go func() {
		isValid = epoch.Check()
		wg.Done()
	}()
	for i := range epoch.jobList {
		go func(i int) {
			epoch.jobList[i].Run(root)
			wg.Done()
		}(i)
	}
	wg.Wait()
	if !isValid {
		fmt.Printf("Found an invalid epoch!")
		return
	}
}

func getShortKey(n uint64) []byte {
	var shortKey [rabbit.KeySize]byte
	binary.LittleEndian.PutUint64(shortKey[:], n)
	return shortKey[:]
}

func (job *Job) Run(root *store.RootStore) {
	for _, tx := range job.txList {
		job.executeTx(root, tx)
	}
}

func GetCachedValue(root *store.RootStore, key []byte) *rabbit.CachedValue {
	bz := root.Get(key)
	if len(bz) == 0 {
		fmt.Printf("Cannot find account %#v\n", key)
		return nil
	}
	return rabbit.BytesToCachedValue(bz)
}

func (job *Job) executeTx(root *store.RootStore, tx Tx) {
	var fromAcc, toAcc Account
	root.PrepareForUpdate(tx.FromNum[:])
	fromAccWrap := GetCachedValue(root, tx.FromNum[:])
	if fromAccWrap == nil {
		fmt.Printf("Cannot find from-account %#v\n", tx.FromNum)
		return
	}
	root.PrepareForUpdate(tx.ToNum[:])
	toAccWrap := GetCachedValue(root, tx.ToNum[:])
	if toAccWrap == nil {
		fmt.Printf("Cannot find to-account %#v\n", tx.ToNum)
		return
	}
	fromAcc.FromBytes(fromAccWrap.GetValue())
	toAcc.FromBytes(toAccWrap.GetValue())
	fromIdx := fromAcc.Find(tx.CoinID)
	if fromIdx < 0 {
		fmt.Printf("Cannot find the token in from-account\n")
		return
	}
	toIdx := toAcc.Find(tx.CoinID)
	if toIdx < 0 {
		fmt.Printf("Cannot find the token in to-account\n")
		return
	}
	amount := int64(binary.LittleEndian.Uint64(tx.Amount[:]))
	if amount < 0 {
		amount = -amount
	}
	nativeTokenAmount, fromAmount, toAmount, toNewAmount := &big.Int{}, &big.Int{}, &big.Int{}, &big.Int{}
	fromAmount.SetBytes(fromAcc.GetTokenAmount(fromIdx))
	toAmount.SetBytes(toAcc.GetTokenAmount(toIdx))
	amountInt := big.NewInt(amount)
	if fromAmount.Cmp(amountInt) < 0 { // not enough tokens
		fmt.Printf("Not enough token")
		return // fail
	}
	fromAmount.Sub(fromAmount, amountInt)
	toNewAmount.Add(toAmount, amountInt)
	if toNewAmount.Cmp(toAmount) < 0 { //overflow
		fmt.Printf("token overflow")
		return // fail
	}
	fromAcc.SetTokenAmount(fromIdx, BigIntToBytes(fromAmount))
	toAcc.SetTokenAmount(toIdx, BigIntToBytes(toAmount))
	nativeTokenAmount.SetBytes(fromAcc.GetNativeAmount())
	gas := big.NewInt(10)
	if nativeTokenAmount.Cmp(gas) < 0 { //overflow
		fmt.Printf("not enough native token for gas")
		return // fail
	}
	nativeTokenAmount.Sub(nativeTokenAmount, gas)
	fromAcc.SetNativeAmount(BigIntToBytes(nativeTokenAmount))
	fromAcc.SetSequence(fromAcc.GetSequence()+1)
	fromAccWrap.SetValue(&fromAcc)
	toAccWrap.SetValue(&toAcc)
	job.changedAccList = append(job.changedAccList, AccountAndNum{fromAccWrap, tx.FromNum})
	job.changedAccList = append(job.changedAccList, AccountAndNum{toAccWrap, tx.ToNum})
}

func RunTx(numBlock int, txFile string) {
	fin, err := os.Open(txFile)
	if err != nil {
		panic(err)
	}
	defer fin.Close()
	ch := make(chan *Block, 2)
	go func() {
		for i := 0; i < numBlock; i++ {
			var block Block
			for j := 0; j < NumEpochInBlock; j++ {
				block.epochList[j] = ReadEpoch(fin)
			}
			ch <- &block
		}
	}()

	fmt.Printf("Start %f\n", float64(time.Now().UnixNano())/1e9)
	okv, err := onvakv.NewOnvaKV("./onvakv4test", false, [][]byte{GuardStart, GuardEnd})
	if err != nil {
		panic(err)
	}
	fmt.Printf("Loaded %f\n", float64(time.Now().UnixNano())/1e9)
	root := store.NewRootStore(okv, nil, nil)
	for i := 0; i < numBlock; i++ {
		fmt.Printf("block %d\n", i)
		root.SetHeight(int64(i))
		block := <-ch
		for j := 0; j < NumEpochInBlock; j++ {
			block.epochList[j].Run(root)
		}
		root.BeginWrite()
		for j := 0; j < NumEpochInBlock; j++ {
			for k := 0; k < NumWorkers; k++ {
				for _, accAndNum := range block.epochList[j].jobList[k].changedAccList {
					root.Set(accAndNum.num[:], accAndNum.acc.ToBytes())
				}
			}
		}
		root.EndWrite()
	}
	root.Close()
	fmt.Printf("Finished %f\n", float64(time.Now().UnixNano())/1e9)
}
