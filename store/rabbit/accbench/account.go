package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"sync"

	sha256 "github.com/minio/sha256-simd"
	"github.com/coinexchain/randsrc"

	"github.com/coinexchain/onvakv"
	"github.com/coinexchain/onvakv/store"
	"github.com/coinexchain/onvakv/store/types"
	"github.com/coinexchain/onvakv/store/rabbit"
)

var (
	GuardStart = []byte{0, 0, 0, 0, 0, 0, 0, 0}
	GuardEnd = []byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255}
)

const (
	MaxCoinCount = 20 // maximum count of coin types in an account
	NumCoinType = 100 // total coin types in the system

	AddrLen = 20
	ShortIDLen = 8
	AmountLen = 32
	EntryLen = ShortIDLen + AmountLen
	AddressOffset = 0
	SequenceOffset = AddressOffset + AddrLen
	NativeTokenAmountOffset = SequenceOffset + 8
	ERC20TokenOffset = NativeTokenAmountOffset + AmountLen

	NumNewAccountsInBlock = 20000
	NumNewAccountsPerWorker = 400
	NumWorkersInBlock = NumNewAccountsInBlock/NumNewAccountsPerWorker
)

// convert coin type as an integer to coin id as a hash
func CoinTypeToCoinID(i int) (res [ShortIDLen]byte) {
	hash := sha256.Sum256([]byte(fmt.Sprintf("coin%d", i)))
	copy(res[:], hash[:])
	return
}

var CoinIDList [NumCoinType][ShortIDLen]byte

func init() { //initialize CoinIDList
	for i := range CoinIDList {
		CoinIDList[i] = CoinTypeToCoinID(i)
	}
}

type Coin struct {
	ID     [ShortIDLen]byte
	Amount [AmountLen]byte
}

type Account struct {
	bz        []byte //all the operations directly read/write raw bytes
	coinCount int
}

var _ types.Serializable = &Account{}

func (acc *Account) ToBytes() []byte {
	return acc.bz
}

func (acc *Account) FromBytes(bz []byte) {
	if len(bz) < ERC20TokenOffset || len(bz[ERC20TokenOffset:]) % EntryLen != 0 {
		panic(fmt.Sprintf("Invalid bytes for Account: %#v", bz))
	}
	acc.bz = bz
	acc.coinCount = len(bz[ERC20TokenOffset:]) / EntryLen
}

func (acc *Account) DeepCopy() interface{} {
	return &Account{
		bz:        append([]byte{}, acc.bz...),
		coinCount: acc.coinCount,
	}
}

func NewAccount(addr [AddrLen]byte, sequence int64, nativeTokenAmount [AmountLen]byte, coins []Coin) Account {
	bz := make([]byte, AddrLen+8+AmountLen+len(coins)*EntryLen)
	copy(bz[AddressOffset:], addr[:])
	binary.LittleEndian.PutUint64(bz[SequenceOffset:SequenceOffset+8], uint64(sequence))
	copy(bz[NativeTokenAmountOffset:], nativeTokenAmount[:])
	start := ERC20TokenOffset
	if len(coins) > MaxCoinCount {
		panic("Too many coins")
	}
	for _, coin := range coins {
		copy(bz[start:], coin.ID[:])
		copy(bz[start+ShortIDLen:], coin.Amount[:])
		start += EntryLen
	}
	return Account{bz: bz, coinCount: len(coins)}
}

func (acc *Account) GetCoinCount() int {
	return acc.coinCount
}

func (acc Account) GetInfo() string {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("Account %#v seq %x %d\n", acc.Address(), acc.GetSequence(), acc.GetSequence()>>32))
	for i := 0; i < acc.coinCount; i++ {
		buffer.WriteString(fmt.Sprintf("   Coin#%d %#v\n", i, acc.GetTokenID(i)))
	}
	return buffer.String()
}

func (acc Account) Address() []byte {
	return acc.bz[AddressOffset:AddressOffset+AddrLen]
}

func (acc Account) GetSequence() int64 {
	return int64(binary.LittleEndian.Uint64(acc.bz[SequenceOffset:SequenceOffset+8]))
}

func (acc Account) SetSequence(seq int64) {
	binary.LittleEndian.PutUint64(acc.bz[SequenceOffset:SequenceOffset+8], uint64(seq))
}

func (acc Account) GetNativeAmount() []byte {
	return acc.bz[NativeTokenAmountOffset:NativeTokenAmountOffset+AmountLen]
}

func (acc Account) SetNativeAmount(amount [AmountLen]byte) {
	copy(acc.bz[NativeTokenAmountOffset:], amount[:])
}

func (acc Account) GetTokenID(i int) (res [ShortIDLen]byte) {
	start := ERC20TokenOffset + i*EntryLen
	copy(res[:], acc.bz[start:start+ShortIDLen])
	return
}

func (acc Account) GetTokenAmount(i int) []byte {
	start := ERC20TokenOffset + i*EntryLen + ShortIDLen
	return acc.bz[start:start+AmountLen]
}

func (acc Account) SetTokenAmount(i int, amount [AmountLen]byte) {
	start := ERC20TokenOffset + i*EntryLen + ShortIDLen
	copy(acc.bz[start:], amount[:])
}

func (acc Account) Find(tokenID [ShortIDLen]byte) int {
	i := sort.Search(acc.coinCount, func(i int) bool {
		id := acc.GetTokenID(i)
		return bytes.Compare(id[:], tokenID[:]) >= 0
	})
	id := acc.GetTokenID(i)
	if i < acc.coinCount && bytes.Equal(id[:], tokenID[:]) { //present
		return i
	} else { // not present
		return -1
	}
}

// the serial number of an account decides its address
func SNToAddr(accountSN int64) (addr [AddrLen]byte) {
	hash := sha256.Sum256([]byte(fmt.Sprintf("address%d", accountSN)))
	copy(addr[:], hash[:])
	return
}

// the serial number of an account decides which types of coins it has
func GetCoinList(accountSN int64) []uint8 {
	hash := sha256.Sum256([]byte(fmt.Sprintf("listofcoins%d", accountSN)))
	coinCount := 1 + hash[0]%MaxCoinCount
	res := make([]uint8, coinCount)
	for i := range res {
		res[i] = hash[i+1] % NumCoinType
	}
	return res
}

func BigIntToBytes(i *big.Int) (res [AmountLen]byte) {
	bz := i.Bytes()
	if len(bz) > AmountLen {
		panic("Too large")
	}
	startingZeros := len(res)-len(bz)
	copy(res[startingZeros:], bz)
	return res
}

func GetRandAmount(rs randsrc.RandSrc) [AmountLen]byte {
	i := big.NewInt(int64(rs.GetUint32()))
	i.Lsh(i, 128)
	return BigIntToBytes(i)
}

func GenerateZeroCoinAccount(accountSN int64) Account {
	var zero [AmountLen]byte
	coinList := GetCoinList(accountSN)
	coins := make([]Coin, len(coinList))
	for i := range coins {
		coinType := int(coinList[i])
		coins[i].ID = CoinIDList[coinType]
	}
	sort.Slice(coins, func(i, j int) bool {
		return bytes.Compare(coins[i].ID[:], coins[j].ID[:]) < 0
	})
	return NewAccount(SNToAddr(accountSN), accountSN<<32, zero, coins)
}

func GenerateAccount(accountSN int64, rs randsrc.RandSrc) Account {
	nativeTokenAmount := GetRandAmount(rs)
	coinList := GetCoinList(accountSN)
	coins := make([]Coin, len(coinList))
	for i := range coins {
		coinType := int(coinList[i])
		coins[i].ID = CoinIDList[coinType]
		coins[i].Amount = GetRandAmount(rs)
	}
	sort.Slice(coins, func(i, j int) bool {
		return bytes.Compare(coins[i].ID[:], coins[j].ID[:]) < 0
	})
	return NewAccount(SNToAddr(accountSN), accountSN<<32, nativeTokenAmount, coins)
}

func RunGenerateAccounts(numAccounts int, randFilename string, jsonFile string) {
	addr2num := make(map[[AddrLen]byte]uint64)
	rs := randsrc.NewRandSrcFromFile(randFilename)
	okv, err := onvakv.NewOnvaKV("./onvakv4test", false, [][]byte{GuardStart, GuardEnd})
	if err != nil {
		panic(err)
	}
	root := store.NewRootStore(okv, nil, nil)

	if numAccounts % NumNewAccountsInBlock != 0 {
		panic("numAccounts % NumNewAccountsInBlock != 0")
	}
	numBlocks := numAccounts / NumNewAccountsInBlock
	for i := 0; i < numBlocks; i++ {
		root.SetHeight(int64(i))
		if i % 10 == 0 {
			fmt.Printf("Now %d of %d, %d\n", i, numBlocks, root.ActiveCount())
		}
		trunk := root.GetTrunkStore().(*store.TrunkStore)
		GenerateAccountsInBlock(i*NumNewAccountsInBlock, trunk, rs, addr2num)
		trunk.Close(true)
	}

	b, err := json.Marshal(addr2num)
	out, err := os.OpenFile(jsonFile, os.O_RDWR|os.O_CREATE, 0700)
	if err != nil {
		panic(err)
	}
	fmt.Printf("len(addr2num): %d, addr2num: %s\n", len(addr2num), string(b))
	out.Write(b)
	out.Close()

	root.Close()
}

func GenerateAccountsInBlock(startIdx int, trunk *store.TrunkStore, rs randsrc.RandSrc, addr2num map[[AddrLen]byte]uint64) {
	var accounts [NumWorkersInBlock][NumNewAccountsPerWorker]Account
	accSN := int64(startIdx)
	for i := 0; i < NumWorkersInBlock; i++ {
		for j := 0; j < NumNewAccountsPerWorker; j++ {
			accounts[i][j] = GenerateAccount(accSN, rs)
			accSN++
		}
	}
	var rbtList [NumWorkersInBlock]rabbit.RabbitStore
	// Parrallel execution
	var wg sync.WaitGroup
	wg.Add(len(rbtList))
	for i := 0; i < NumWorkersInBlock; i++ {
		go func(i int) {
			rbt := rabbit.NewRabbitStore(trunk)
			rbtList[i] = rbt
			WriteAccounts(accounts, i, rbt, addr2num)
			wg.Done()
		}(i)
	}
	wg.Wait()
	// Serial collection
	touchedShortKey := make(map[[rabbit.KeySize]byte]struct{}, NumNewAccountsInBlock)
	for i, rbt := range rbtList {
		hasConflict := false
		rbt.ScanAllShortKeys(func(key [rabbit.KeySize]byte) (stop bool) {
			if _, ok := touchedShortKey[key]; ok {
				hasConflict = true
				return true
			}
			return false
		})
		if hasConflict { // re-execute it serially
			fmt.Printf("hasConflict %d\n", i)
			rbt.Close(false)
			rbt = rabbit.NewRabbitStore(trunk)
			WriteAccounts(accounts, i, rbt, addr2num)
		}
		rbt.ScanAllShortKeys(func(key [rabbit.KeySize]byte) (stop bool) {
			touchedShortKey[key] = struct{}{}
			return false
		})
		rbt.Close(true)
	}
}

func WriteAccounts(accounts [NumWorkersInBlock][NumNewAccountsPerWorker]Account, i int, rbt rabbit.RabbitStore, addr2num map[[AddrLen]byte]uint64) {
	for _, acc := range accounts[i] {
		rbt.Set(acc.Address(), acc.ToBytes())
		path, ok := rbt.GetShortKeyPath(acc.Address())
		if !ok {
			panic("Cannot get the object which was just set")
		}
		if len(path) > 1 { // special cases: more than one hop
			var addr [AddrLen]byte
			copy(addr[:], acc.Address())
			n := binary.LittleEndian.Uint64(path[len(path)-1][:])
			addr2num[addr] = n
		}
	}
}