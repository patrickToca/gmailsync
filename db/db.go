package db

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"
)

var _ = log.Printf
var _ = fmt.Printf

const (
	AnyType = iota
	MessageRecordType
	LabelsRecordType
	DeleteRecordType
	HaveRecordType
)

type DB struct {
	sync.Mutex
	labels        map[int64][]string
	labelsChanged map[int64]bool
	haveMsgID     map[int64]bool
	fd            *os.File
}

const (
	FeatureCompressed = 1 << iota
	FeatureHashed
)

type Header struct {
	Type        uint16
	FeatureBits uint16
	Length      uint32
}

type MessageRecord struct {
	MessageID int64
	Data      []byte
}

type LabelsRecord []LabelsEntry

type LabelsEntry struct {
	MessageID int64
	Labels    [][]byte
}

const fileMagic = 0x20121025

var fileHeaderLength = binary.Size(FileHeader{})

type FileHeader struct {
	Magic      uint32
	Version    uint8
	Reserved0  uint8
	Reserved1  uint16
	CreateTime uint32
	UpdateTime uint32
	HavePtr    uint64
	Reserved2  uint64
	Reserved3  uint64
}

func Open(name string) (*DB, error) {
	var db DB
	var err error

	db.labels = make(map[int64][]string)
	db.labelsChanged = make(map[int64]bool)
	db.haveMsgID = make(map[int64]bool)

	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}

	db.fd = f

	var fhdr FileHeader
	if stat, err := f.Stat(); err == nil && stat.Size() == 0 {
		// New file, write magic
		fhdr = FileHeader{
			Magic:      fileMagic,
			Version:    1,
			CreateTime: uint32(time.Now().Unix()),
		}
		binary.Write(db.fd, binary.LittleEndian, fhdr)
	} else {
		binary.Read(db.fd, binary.LittleEndian, &fhdr)
		if fhdr.Magic != fileMagic {
			return nil, errors.New("Incorrect file format")
		}
	}

	for {
		rec, err := db.nextRecord(AnyType)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch trec := rec.(type) {
		case MessageRecord:
			db.haveMsgID[trec.MessageID] = true
		case LabelsRecord:
			for _, lrec := range trec {
				db.labels[lrec.MessageID] = bytesSliceToStrings(lrec.Labels)
			}
		}
	}

	db.Rewind()
	return &db, nil
}

func stringSliceToBytes(ss []string) [][]byte {
	var res [][]byte
	for _, s := range ss {
		res = append(res, []byte(s))
	}
	return res
}

func bytesSliceToStrings(bs [][]byte) []string {
	var res []string
	for _, b := range bs {
		res = append(res, string(b))
	}
	return res
}

func (db *DB) Rewind() {
	db.fd.Seek(int64(fileHeaderLength), os.SEEK_SET)
}

func (db *DB) Size() int {
	defer db.Unlock()
	db.Lock()
	return len(db.haveMsgID)
}

func (db *DB) HaveUID(msgid int64) bool {
	defer db.Unlock()
	db.Lock()
	return db.haveMsgID[msgid]
}

func (db *DB) Labels(msgid int64) []string {
	defer db.Unlock()
	db.Lock()
	return db.labels[msgid]
}

func (db *DB) SetLabels(msgid int64, labels []string) {
	defer db.Unlock()
	db.Lock()
	db.labels[msgid] = labels
	db.labelsChanged[msgid] = true
}

func (db *DB) WriteMessage(msgid int64, data []byte) error {
	rec := MessageRecord{MessageID: msgid, Data: data}
	bs, err := asn1.Marshal(rec)
	if err != nil {
		panic(err)
	}

	defer db.Unlock()
	db.Lock()

	return db.writeRecord(MessageRecordType, FeatureCompressed|FeatureHashed, bs)
}

func (db *DB) ReadMessage() (*MessageRecord, error) {
	intf, err := db.nextRecord(MessageRecordType)
	if err != nil {
		return nil, err
	}
	rec := intf.(MessageRecord)
	return &rec, nil
}

func (db *DB) WriteLabels() error {
	var lbls LabelsRecord

	defer db.Unlock()
	db.Lock()

	for msgid := range db.labelsChanged {
		rec := LabelsEntry{MessageID: msgid, Labels: stringSliceToBytes(db.labels[msgid])}
		lbls = append(lbls, rec)
	}
	db.labelsChanged = make(map[int64]bool)

	bs, err := asn1.Marshal(lbls)
	if err != nil {
		panic(err)
	}

	return db.writeRecord(LabelsRecordType, FeatureCompressed, bs)
}

func (db *DB) nextRecord(recordType uint16) (interface{}, error) {
	for {
		var hdr Header
		err := binary.Read(db.fd, binary.LittleEndian, &hdr)
		if err != nil {
			return nil, err
		}

		if recordType != AnyType && hdr.Type != recordType {
			db.fd.Seek(int64(hdr.Length), os.SEEK_CUR)
			continue
		}

		var data = make([]byte, hdr.Length)
		_, err = db.fd.Read(data)
		if err != nil {
			return nil, err
		}

		var dhash []byte
		if hdr.FeatureBits&FeatureHashed != 0 {
			dhash = data[:20]
			data = data[20:]
		}

		if hdr.FeatureBits&FeatureCompressed != 0 {
			data = decompress(data)
		}

		if hdr.FeatureBits&FeatureHashed != 0 {
			chash := hash(data)
			if bytes.Compare(chash, dhash) != 0 {
				panic("hash failure")
			}
		}

		switch hdr.Type {
		case MessageRecordType:
			var msg MessageRecord
			_, err := asn1.Unmarshal(data, &msg)
			if err != nil {
				panic(err)
			}
			return msg, err

		case LabelsRecordType:
			var lbl LabelsRecord
			_, err := asn1.Unmarshal(data, &lbl)
			if err != nil {
				panic(err)
			}
			return lbl, err
		}
	}
}

func (db *DB) writeRecord(rtype uint16, features uint16, data []byte) error {
	var bs []byte

	if features&FeatureHashed != 0 {
		bs = append(bs, hash(data)...)
	}
	if features&FeatureCompressed != 0 {
		bs = append(bs, compress(data)...)
	} else {
		bs = append(bs, data...)
	}

	hdr := Header{rtype, features, uint32(len(bs))}

	db.fd.Seek(0, os.SEEK_END)
	binary.Write(db.fd, binary.LittleEndian, hdr)
	db.fd.Write(bs)
	return db.fd.Sync()
}

func compress(bs []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(bs)
	gz.Close()
	return buf.Bytes()
}

func hash(bs []byte) []byte {
	ha := sha1.New()
	ha.Write(bs)
	return ha.Sum(nil)
}

func decompress(bs []byte) []byte {
	gz, err := gzip.NewReader(bytes.NewBuffer(bs))
	if err != nil {
		panic(err)
	}

	data, err := ioutil.ReadAll(gz)
	if err != nil {
		panic(err)
	}
	return data
}
