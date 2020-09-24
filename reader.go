package dsstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

func readBlock(fileData []byte, offset, size uint32) *bytes.Buffer {
	// check size
	if offset+4+size > uint32(len(fileData)) {
		return nil
	}
	// alloc reading buffer
	return bytes.NewBuffer(fileData[offset+4 : offset+4+size])
}

func readOffsets(b *bytes.Buffer) ([]uint32, error) {
	var count uint32
	if err := binary.Read(b, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	// read dummy value
	var value uint32
	if err := binary.Read(b, binary.BigEndian, &value); err != nil {
		return nil, err
	}
	// read offsets
	offsets := make([]uint32, 0)
	for offcount := int(count); offcount > 0; offcount -= 256 {
		for i := 0; i < 256; i++ {
			if err := binary.Read(b, binary.BigEndian, &value); err != nil {
				return nil, err
			}
			if value == 0 {
				continue
			}
			offsets = append(offsets, value)
		}
	}
	// offsets
	return offsets, nil
}

func readTopics(b *bytes.Buffer) (map[string]uint32, error) {
	// read topic count
	var count uint32
	if err := binary.Read(b, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	// read topics
	topics := make(map[string]uint32)
	for i := count; i > 0; i-- {
		// read topic len
		len, err := b.ReadByte()
		if err != nil {
			return nil, err
		}
		// read topic name
		name := make([]byte, len)
		_, err = b.Read(name)
		if err != nil {
			return nil, err
		}
		// read topic index
		var index uint32
		if err := binary.Read(b, binary.BigEndian, &index); err != nil {
			return nil, err
		}
		// add topic
		topics[string(name)] = index
	}
	return topics, nil
}

func readFreeBlocks(b *bytes.Buffer) error {
	for i := 0; i < 32; i++ {
		var count uint32
		if err := binary.Read(b, binary.BigEndian, &count); err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		for k := 0; k < int(count); k++ {
			var value uint32
			if err := binary.Read(b, binary.BigEndian, &value); err != nil {
				return err
			}
		}
	}
	return nil
}

func readParseFile(b *bytes.Buffer) (Record, error) {
	r := Record{}
	// len
	var len uint32
	if err := binary.Read(b, binary.BigEndian, &len); err != nil {
		return r, err
	}
	// name
	name16 := make([]byte, 2*len)
	if _, err := b.Read(name16); err != nil {
		return r, err
	}
	// extra
	if err := binary.Read(b, binary.BigEndian, &r.Extra); err != nil {
		return r, err
	}
	// type
	stype := make([]byte, 4)
	if _, err := b.Read(stype); err != nil {
		return r, err
	}
	r.Type = string(stype)

	byteToRead := -1
	// read data
	switch {
	case r.Type == "bool":
		byteToRead = 1
		break
	case r.Type == "type" || r.Type == "long" || r.Type == "shor":
		byteToRead = 4
		break
	case r.Type == "comp" || r.Type == "dutc":
		byteToRead = 8
		break
	case r.Type == "blob":
		if err := binary.Read(b, binary.BigEndian, &r.DataLen); err != nil {
			return r, err
		}
		byteToRead = int(r.DataLen)
		break
	case r.Type == "ustr":
		if err := binary.Read(b, binary.BigEndian, &r.DataLen); err != nil {
			return r, err
		}
		byteToRead = int(2 * r.DataLen)
	default:
		break
	}
	if byteToRead <= 0 {
		return r, fmt.Errorf("unknown record format [%s]", r.Type)
	}
	r.Data = make([]byte, byteToRead)
	if _, err := b.Read(r.Data); err != nil {
		return r, err
	}
	name, _, err := transform.Bytes(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder(), name16)
	if err != nil {
		return r, err
	}
	r.FileName = string(name)
	return r, nil
}

func (s *Store) readParseData(fileData []byte, offsets []uint32, node uint32) error {
	// check node
	if int(node) >= len(offsets) {
		return errors.New("invalid data block")
	}
	// prepare data block
	offset := offsets[node]
	blockData := readBlock(fileData, blockOffset(offset), blockSize(offset))
	if blockData == nil {
		return errors.New("invalid data block")
	}

	var nextNode uint32
	if err := binary.Read(blockData, binary.BigEndian, &nextNode); err != nil {
		return err
	}
	var count uint32
	if err := binary.Read(blockData, binary.BigEndian, &count); err != nil {
		return err
	}

	if nextNode > 0 {
		for i := 0; i < int(count); i++ {
			var childNode uint32
			if err := binary.Read(blockData, binary.BigEndian, &childNode); err != nil {
				return err
			}
			if err := s.readParseData(fileData, offsets, childNode); err != nil {
				return err
			}
			// get the file for the current block
			r, err := readParseFile(blockData)
			if err != nil {
				return err
			}
			s.Records = append(s.Records, r)
		}
		err := s.readParseData(fileData, offsets, nextNode)
		if err != nil {
			return err
		}
	} else {
		for i := 0; i < int(count); i++ {
			r, err := readParseFile(blockData)
			if err != nil {
				return err
			}
			s.Records = append(s.Records, r)
		}
	}
	return nil
}

func (s *Store) readParseDSDB(fileData []byte, offsets []uint32, topics map[string]uint32) error {
	// find node by topic and check it
	node := topics["DSDB"]
	if int(node) >= len(offsets) {
		return errors.New("invalid DSDB block")
	}
	// find topic block
	offset := offsets[node]
	blockDSDB := readBlock(fileData, blockOffset(offset), blockSize(offset))
	if blockDSDB == nil {
		return errors.New("invalid DSDB block")
	}
	// read data node
	var dataNode uint32
	err := binary.Read(blockDSDB, binary.BigEndian, &dataNode)
	if err != nil {
		return err
	}
	// read extra
	if s.DSDBExtra, err = ioutil.ReadAll(blockDSDB); err != nil {
		return err
	}
	// parse data
	return s.readParseData(fileData, offsets, dataNode)
}

func (s *Store) readParseRoot(fileData []byte, offset, size uint32) error {
	blockRoot := readBlock(fileData, offset, size)
	if blockRoot == nil {
		return errors.New("invalid root block")
	}
	// read offsets
	offsets, err := readOffsets(blockRoot)
	if err != nil {
		return err
	}
	// read topics
	topics, err := readTopics(blockRoot)
	if err != nil {
		return err
	}
	// parse free blocks
	if err = readFreeBlocks(blockRoot); err != nil {
		return err
	}
	// read extra root data
	if s.RootExtra, err = ioutil.ReadAll(blockRoot); err != nil {
		return err
	}
	// parse DSDB
	return s.readParseDSDB(fileData, offsets, topics)
}

// Read is reads .DS_Store
func (s *Store) Read(r io.Reader) error {
	// clear
	s.HeaderExtra = nil
	s.RootExtra = nil
	s.DSDBExtra = nil
	s.Records = nil
	// read all
	fileData, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	// file size
	fileSize := len(fileData)
	if fileSize < 36 {
		return errors.New("invalid file header")
	}
	blockHeader := bytes.NewBuffer(fileData[:36])
	var headerMagic, headerOffset1, headerSize, headerOffset2 uint32
	// magic 1
	if err := binary.Read(blockHeader, binary.BigEndian, &headerMagic); err != nil {
		return err
	}
	if headerMagic != headerMagic1 {
		return errors.New("invalid first magic")
	}
	// magic 2
	if err := binary.Read(blockHeader, binary.BigEndian, &headerMagic); err != nil {
		return err
	}
	if headerMagic != headerMagic2 {
		return errors.New("invalid second magic")
	}
	// offset1
	if err := binary.Read(blockHeader, binary.BigEndian, &headerOffset1); err != nil {
		return err
	}
	// size
	if err := binary.Read(blockHeader, binary.BigEndian, &headerSize); err != nil {
		return err
	}
	// offset2
	if err := binary.Read(blockHeader, binary.BigEndian, &headerOffset2); err != nil {
		return err
	}
	if headerOffset1 != headerOffset2 {
		return errors.New("invalid header offset")
	}
	// read header extra
	if s.HeaderExtra, err = ioutil.ReadAll(blockHeader); err != nil {
		return err
	}
	// parse root block
	return s.readParseRoot(fileData, headerOffset1, headerSize)
}

// ReadFile is reads .DS_Store
func (s *Store) ReadFile(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Read(f)
}
