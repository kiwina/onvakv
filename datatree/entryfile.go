package datatree

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/mmcloughlin/meow"

	"github.com/coinexchain/onvakv/types"
)

type Entry = types.Entry

const MaxEntryBytes int = (1<<24)-1
var MagicBytes = [8]byte{byte('I'),byte('L'),byte('O'),byte('V'),byte('E'),byte('Y'),byte('O'),byte('U')}

func NullEntry() Entry {
	return Entry{
		Key:       []byte{},
		Value:     []byte{},
		NextKey:   []byte{},
		Height:    -1,
		LastHeight:-1,
		SerialNum: -1,
	}
}

// Entry serialization format:
// magicBytes 8-bytes
// 32b-totalLength without padding, checksum and itself
// magicBytesPos(list of 32b-int, -1 for ending), posistions are relative to the end of 32b-totalLength
// normalPayload
// DeactivedSerialNumList (list of 64b-int, -1 for ending)
// 32b-checksum
// padding-zero-bytes

func EntryToBytes(entry Entry, deactivedSerialNumList []int64) []byte {
	length := 4+4 // 32b-totalLength and empty magicBytesPos
	length += 4*3 + len(entry.Key)+len(entry.Value)+len(entry.NextKey) //Three strings
	length += +8*3 // Three int64
	length += (len(deactivedSerialNumList)+1)*8
	b := make([]byte, length)

	const start = 8
	i := start
	binary.LittleEndian.PutUint32(b[i:i+4], uint32(len(entry.Key)))
	i += 4
	copy(b[i:i+length], entry.Key)
	i += len(entry.Key)

	binary.LittleEndian.PutUint32(b[i:i+4], uint32(len(entry.Value)))
	i += 4
	copy(b[i:i+length], entry.Value)
	i += len(entry.Value)

	binary.LittleEndian.PutUint32(b[i:i+4], uint32(len(entry.NextKey)))
	i += 4
	copy(b[i:i+length], entry.NextKey)
	i += len(entry.NextKey)

	binary.LittleEndian.PutUint64(b[i:i+8], uint64(entry.Height))
	i += 8
	binary.LittleEndian.PutUint64(b[i:i+8], uint64(entry.LastHeight))
	i += 8
	binary.LittleEndian.PutUint64(b[i:i+8], uint64(entry.SerialNum))
	i += 8

	for _, sn := range deactivedSerialNumList {
		binary.LittleEndian.PutUint64(b[i:i+8], uint64(sn))
		i += 8
	}
	binary.LittleEndian.PutUint64(b[i:i+8], math.MaxUint64)

	magicBytesPosList := getAllPos(b, MagicBytes[:], start)
	if len(magicBytesPosList) ==0 {
		binary.LittleEndian.PutUint32(b[:4], uint32(length-4))
		binary.LittleEndian.PutUint32(b[4:8], ^uint32(0))
		return b
	}

	var zeroBuf [8]byte
	for _, pos := range magicBytesPosList {
		copy(b[pos:pos+8], zeroBuf[:])
	}
	length += 4 * len(magicBytesPosList)
	buf := make([]byte, length)
	binary.LittleEndian.PutUint32(buf[:4], uint32(length-4))
	bytesAdded := 4 * (len(magicBytesPosList) - 1)
	for i=0; i < len(magicBytesPosList); i++ {
		pos := magicBytesPosList[i]+bytesAdded-4/*32b-length*/
		binary.LittleEndian.PutUint32(buf[i*4+4:i*4+8], uint32(pos))
	}
	binary.LittleEndian.PutUint32(buf[i*4+4:i*4+8], ^uint32(0))
	copy(buf[i*4+8:], b[8:])
	return buf
}

func getAllPos(s, sep []byte, start int) (allpos []int) {
	for start < len(s) {
		pos := bytes.Index(s[start:], sep)
		if pos == -1 {
			return
		}
		allpos = append(allpos, pos + start)
		start = pos+len(sep)
	}
	return
}

func EntryFromBytes(b []byte) (*Entry, []uint64) {
	entry := &Entry{}
	i := 0

	length := int(binary.LittleEndian.Uint32(b[i:i+4]))
	i += 4
	entry.Key = append([]byte{}, b[i:i+length]...)
	i += length

	length = int(binary.LittleEndian.Uint32(b[i:i+4]))
	i += 4
	entry.Value = append([]byte{}, b[i:i+length]...)
	i += length

	length = int(binary.LittleEndian.Uint32(b[i:i+4]))
	i += 4
	entry.NextKey = append([]byte{}, b[i:i+length]...)
	i += length

	entry.Height = int64(binary.LittleEndian.Uint64(b[i:i+8]))
	i += 8
	entry.LastHeight = int64(binary.LittleEndian.Uint64(b[i:i+8]))
	i += 8
	entry.SerialNum = int64(binary.LittleEndian.Uint64(b[i:i+8]))
	i += 8

	var deactivedSerialNumList []uint64
	sn := binary.LittleEndian.Uint64(b[i:i+8])
	for sn != math.MaxUint64 {
		deactivedSerialNumList = append(deactivedSerialNumList, sn)
		i += 8
		sn = binary.LittleEndian.Uint64(b[i:i+8])
	}

	return entry, deactivedSerialNumList
}

type EntryFile struct {
	HPFile
}

func getPaddingSize(length int) int {
	rem := length%8
	if rem == 0 {
		return 0
	} else {
		return 8 - rem
	}
}

func (ef *EntryFile) readMagicBytesAndLength(off int64) int {
	var buf [8]byte
	err := ef.HPFile.ReadAt(buf[:], off)
	if err != nil {
		panic(err)
	}
	if !bytes.Equal(buf[:], MagicBytes[:]) {
		panic("Invalid MagicBytes")
	}
	err = ef.HPFile.ReadAt(buf[:4], off+8)
	if err != nil {
		panic(err)
	}
	length := int(binary.LittleEndian.Uint32(buf[:4]))
	if length >= MaxEntryBytes {
		panic("Entry to long")
	}
	return length
}

func (ef *EntryFile) SkipEntry(off int64) int64 {
	length := ef.readMagicBytesAndLength(off)
	paddingSize := getPaddingSize(length)
	nextPos := off + 8/*magicbytes*/ + 4/*length*/ + int64(length) + 4/*checksum*/ + int64(paddingSize)
	return nextPos
}

func (ef *EntryFile) ReadEntry(off int64) (*Entry, []uint64, int64) {
	length := ef.readMagicBytesAndLength(off)
	paddingSize := getPaddingSize(length)
	nextPos := off + 8/*magicbytes*/ + 4/*length*/ + int64(length) + 4/*checksum*/ + int64(paddingSize)
	b := make([]byte, length + 4 + paddingSize)
	err := ef.HPFile.ReadAt(b, off+12)
	if err != nil {
		panic(err)
	}
	h := meow.New32(0)
	h.Write(b[:length])
	if !bytes.Equal(b[length:length+4], h.Sum(nil)) {
		panic("Checksum Error")
	}
	var n int
	for n=0; n<length; n+=4 {
		pos := binary.LittleEndian.Uint32(b[n:n+4])
		if pos == ^(uint32(0)) {
			break
		}
		if int(pos) >= MaxEntryBytes {
			panic("Position to large")
		}
		copy(b[int(pos):int(pos)+8], MagicBytes[:])
	}
	entry, deactivedSerialNumList := EntryFromBytes(b[n:length])
	return entry, deactivedSerialNumList, nextPos
}

func NewEntryFile(blockSize int, dirName string) (res EntryFile, err error) {
	res.HPFile, err = NewHPFile(blockSize, dirName)
	return
}

func (ef *EntryFile) Size() int64 {
	return ef.HPFile.Size()
}
func (ef *EntryFile) Truncate(size int64) {
	err := ef.HPFile.Truncate(size)
	if err != nil {
		panic(err)
	}
}
func (ef *EntryFile) Sync() {
	err := ef.HPFile.Sync()
	if err != nil {
		panic(err)
	}
}
func (ef *EntryFile) Close() {
	err := ef.HPFile.Close()
	if err != nil {
		panic(err)
	}
}
func (ef *EntryFile) PruneHead(off int64) {
	err := ef.HPFile.PruneHead(off)
	if err != nil {
		panic(err)
	}
}
func (ef *EntryFile) Append(b []byte) (pos int64) {
	var bb bytes.Buffer
	bb.Write(MagicBytes[:])
	bb.Write(b)
	h := meow.New32(0)
	h.Write(b)
	bb.Write(h.Sum(nil))
	padding := make([]byte, getPaddingSize(len(b)))
	bb.Write(padding)
	pos, err := ef.HPFile.Append(bb.Bytes())
	if pos%8 != 0 {
		panic("Entries are not aligned")
	}
	if err != nil {
		panic(err)
	}
	return
}

func (ef *EntryFile) GetActiveEntriesInTwig(twig *Twig) (res []*Entry) {
	start := twig.FirstEntryPos
	for i:=0; i < LeafCountInTwig; i++ {
		if twig.getBit(i) {
			entry, _, next := ef.ReadEntry(start)
			start = next
			res = append(res, entry)
		} else {
			start = ef.SkipEntry(start)
		}
	}
	return
}

