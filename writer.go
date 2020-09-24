package dsstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sort"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

type dsblock struct {
	offset uint32
	size   uint32
}

func writeBlockMapCreate() []dsblock {
	blockList := make([]dsblock, 0)
	for i := 5; i < 31; i++ {
		pow := uint32(1 << i)
		blockList = append(blockList, dsblock{pow, pow})
	}
	return blockList
}

func writeBlockMapSort(blockList []dsblock) {
	sort.SliceStable(blockList, func(i, j int) bool {
		if blockList[i].size < blockList[j].size {
			return true
		}
		if blockList[i].size < blockList[j].size {
			return blockList[i].offset < blockList[j].offset
		}
		return false
	})
}

func writeBlockMapAlloc(blockList []dsblock, size uint32, capacity uint32) (uint32, []dsblock) {
	// check capacity
	if capacity < size {
		capacity = size
	}
	// calc size powered by 2
	var powCapacity uint32 = 0
	var powSize uint32 = 0
	var powIndex uint32 = 32
	for i := uint32(5); i < 32; i++ {
		powCapacity = uint32(1) << i
		if powCapacity >= size && powIndex >= 32 {
			powIndex = i
			powSize = powCapacity
		}
		if powCapacity >= capacity {
			if powIndex < 32 {
				break
			}
		}
	}
	// sort map
	writeBlockMapSort(blockList)
	// find block in usage blocks
	for i, block := range blockList {
		if block.size != block.offset && block.size == powCapacity {
			blockList = append(blockList[:i], blockList[i+1:]...)
			if block.size > powSize {
				blockList = append(blockList, dsblock{block.offset + powSize, block.size - powSize})
			}
			return block.offset | powIndex, blockList
		}
	}
	// find block
	for i, block := range blockList {
		if block.size >= powCapacity {
			blockList = append(blockList[:i], blockList[i+1:]...)
			if block.size > powSize {
				blockList = append(blockList, dsblock{block.offset + powSize, block.size - powSize})
			}
			return block.offset | powIndex, blockList
		}
	}
	return 0, blockList
}

func writeAlignBlock(b *bytes.Buffer, minSize uint32) error {
	var len uint32 = uint32(b.Len())
	for i := 0; i < 32; i++ {
		var aligned uint32 = 1 << i
		if len <= aligned && minSize <= aligned {
			// add dummy bytes
			if _, err := b.Write(make([]byte, aligned-len)); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func writeBlockData(b *bytes.Buffer, records []Record) error {
	// nextBlock is always 0. Storing all records in the one block
	if err := binary.Write(b, binary.BigEndian, uint32(0)); err != nil {
		return err
	}
	// count of records
	if err := binary.Write(b, binary.BigEndian, uint32(len(records))); err != nil {
		return err
	}
	// records
	for _, r := range records {
		// r.FileName
		n, _, err := transform.Bytes(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewEncoder(), []byte(r.FileName))
		if err != nil {
			return err
		}
		if err := binary.Write(b, binary.BigEndian, uint32(len(n)/2)); err != nil {
			return err
		}
		if _, err := b.Write(n); err != nil {
			return err
		}
		// unknown extra 4 bytes
		if err := binary.Write(b, binary.BigEndian, uint32(r.Extra)); err != nil {
			return err
		}
		// r.Type (4-bytes string)
		t := make([]byte, 4)
		copy(t, []byte(r.Type))
		if _, err := b.Write(t); err != nil {
			return err
		}
		// r.DataLen for blob, ustr etc
		if r.DataLen > 0 {
			if err := binary.Write(b, binary.BigEndian, uint32(r.DataLen)); err != nil {
				return err
			}
		}
		// r.Data
		if _, err := b.Write(r.Data); err != nil {
			return err
		}
	}
	return nil
}

func writeBlockDSDB(b *bytes.Buffer, dsdbExtra []byte, index uint32) error {
	// write data block index
	if err := binary.Write(b, binary.BigEndian, index); err != nil {
		return err
	}
	// dummy 16 bytes
	if _, err := b.Write(dsdbExtra); err != nil {
		return err
	}
	return nil
}

func writeOffsets(b *bytes.Buffer, offsetRoot, offsetDBDS, offsetData uint32) error {
	// count of offset. always 3 blocks only
	if err := binary.Write(b, binary.BigEndian, uint32(3)); err != nil {
		return err
	}
	// dummy 4 bytes
	if err := binary.Write(b, binary.BigEndian, uint32(0)); err != nil {
		return err
	}
	// offsets
	if err := binary.Write(b, binary.BigEndian, offsetRoot); err != nil {
		return err
	}
	if err := binary.Write(b, binary.BigEndian, offsetDBDS); err != nil {
		return err
	}
	if err := binary.Write(b, binary.BigEndian, offsetData); err != nil {
		return err
	}
	// dummy 253 zero offsets
	for i := 0; i < 253; i++ {
		if err := binary.Write(b, binary.BigEndian, uint32(0)); err != nil {
			return err
		}
	}
	return nil
}

func writeTopicDBDS(b *bytes.Buffer, index uint32) error {
	// always one topic (DBDS)
	if err := binary.Write(b, binary.BigEndian, uint32(1)); err != nil {
		return err
	}
	// topic name len. Always is 4
	if err := b.WriteByte(4); err != nil {
		return err
	}
	// topic name
	if _, err := b.Write([]byte("DSDB")); err != nil {
		return err
	}
	// topic offset
	if err := binary.Write(b, binary.BigEndian, uint32(index)); err != nil {
		return err
	}
	return nil
}

func writeFreeBlocks(b *bytes.Buffer, freeBlocks []dsblock) error {
	m := make(map[uint32][]uint32)
	//
	writeBlockMapSort(freeBlocks)
	//
	for i := 0; i < 32; i++ {
		var blockSize uint32 = uint32(1) << i
		var blockCount uint32 = 0
		for j := 0; j < len(freeBlocks); j++ {
			power := (freeBlocks[j].size & (freeBlocks[j].size - 1)) == 0
			if freeBlocks[j].size == blockSize || (freeBlocks[j].size > blockSize && !power) {
				blockCount++
				continue
			}
			if freeBlocks[j].size >= blockSize {
				break
			}
		}
		if err := binary.Write(b, binary.BigEndian, uint32(blockCount)); err != nil {
			return err
		}
		if blockCount > 0 {
			m[uint32(i)] = make([]uint32, 0)

			for j := 0; j < len(freeBlocks); j++ {
				power := (freeBlocks[j].size & (freeBlocks[j].size - 1)) == 0
				if freeBlocks[j].size == blockSize || (freeBlocks[j].size > blockSize && !power) {
					m[uint32(i)] = append(m[uint32(i)], freeBlocks[j].offset)
					if err := binary.Write(b, binary.BigEndian, uint32(freeBlocks[j].offset)); err != nil {
						return err
					}
					continue
				}
				if freeBlocks[j].size >= blockSize {
					break
				}
			}
		}
	}

	return nil
}

func writeBlockRoot(b *bytes.Buffer, rootExtra []byte, offsetRoot, offsetDBDS, offsetData uint32, freeBlocks []dsblock) error {
	// offsets
	if err := writeOffsets(b, offsetRoot, offsetDBDS, offsetData); err != nil {
		return err
	}
	// topic. index is always 1
	if err := writeTopicDBDS(b, 1); err != nil {
		return err
	}
	// free blocks
	if err := writeFreeBlocks(b, freeBlocks); err != nil {
		return err
	}
	// write extra (unknown data)
	if _, err := b.Write(rootExtra); err != nil {
		return err
	}
	return nil
}

func (s *Store) writeHeader(b *bytes.Buffer, offsetRoot, size uint32) error {
	// magic1
	if err := binary.Write(b, binary.BigEndian, headerMagic1); err != nil {
		return err
	}
	// magic2
	if err := binary.Write(b, binary.BigEndian, headerMagic2); err != nil {
		return err
	}
	// offset of root block
	if err := binary.Write(b, binary.BigEndian, offsetRoot); err != nil {
		return err
	}
	// size of root block
	if err := binary.Write(b, binary.BigEndian, size); err != nil {
		return err
	}
	// offset of root block
	if err := binary.Write(b, binary.BigEndian, offsetRoot); err != nil {
		return err
	}
	// write extra
	headerExtra := make([]byte, 16)
	copy(headerExtra, s.HeaderExtra)
	if _, err := b.Write(headerExtra); err != nil {
		return err
	}
	// check size
	if b.Len() != 36 {
		return errors.New("invalid header size")
	}
	return nil
}

// WriteStore write .DS_Store
func (s *Store) Write(w io.Writer) error {
	// prepare data block
	blockData := new(bytes.Buffer)
	if err := writeBlockData(blockData, s.Records); err != nil {
		return err
	}
	// prepare DSDB block (always 2 index)
	blockDSDB := new(bytes.Buffer)
	if err := writeBlockDSDB(blockDSDB, s.DSDBExtra, 2); err != nil {
		return err
	}
	// prepare Root block
	blockList := writeBlockMapCreate()
	blockRoot := new(bytes.Buffer)
	if err := writeBlockRoot(blockRoot, make([]byte, 0), 0, 0, 0, blockList); err != nil {
		return err
	}
	// align blocks
	if err := writeAlignBlock(blockData, 32); err != nil {
		return err
	}
	if err := writeAlignBlock(blockDSDB, 32); err != nil {
		return err
	}
	if err := writeAlignBlock(blockRoot, 32); err != nil {
		return err
	}
	// create blocks map
	blockDataOffset, blockList := writeBlockMapAlloc(blockList, uint32(blockData.Len()), 0)
	blockDSDBOffset, blockList := writeBlockMapAlloc(blockList, uint32(blockDSDB.Len()), 0)
	blockRootOffset, blockList := writeBlockMapAlloc(blockList, uint32(blockRoot.Len()), 0)
	// calc real offset
	blockDataOffsetReal := blockOffset(blockDataOffset)
	blockDSDBOffsetReal := blockOffset(blockDSDBOffset)
	blockRootOffsetReal := blockOffset(blockRootOffset)
	// calc end of blocks
	blockDataEnd := blockDataOffsetReal + blockSize(blockDataOffset)
	blockDSDBEnd := blockDSDBOffsetReal + blockSize(blockDSDBOffset)
	blockRootEnd := blockRootOffsetReal + blockSize(blockRootOffset)
	// re-create root block with correct offsets
	blockRoot.Reset()
	if err := writeBlockRoot(blockRoot, s.RootExtra, blockRootOffset, blockDSDBOffset, blockDataOffset, blockList); err != nil {
		return err
	}
	// write header
	blockHeader := new(bytes.Buffer)
	if err := s.writeHeader(blockHeader, blockRootOffsetReal, uint32(blockRoot.Len())); err != nil {
		return err
	}
	// calculate file size
	var size uint32 = 32
	if blockRootEnd > size {
		size = blockRootEnd
	}
	if blockDSDBEnd > size {
		size = blockDSDBEnd
	}
	if blockDataEnd > size {
		size = blockDataEnd
	}
	// create full file
	fileData := make([]byte, size+4)
	copy(fileData[0:], blockHeader.Bytes())
	copy(fileData[4+blockRootOffsetReal:], blockRoot.Bytes())
	copy(fileData[4+blockDSDBOffsetReal:], blockDSDB.Bytes())
	copy(fileData[4+blockDataOffsetReal:], blockData.Bytes())
	// write it
	_, err := w.Write(fileData)
	return err
}
