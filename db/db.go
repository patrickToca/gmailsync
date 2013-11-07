package db

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"github.com/calmh/gmailsync/asn1"
)

var _ = log.Printf

const (
	AnyType uint8 = iota
	MessageRecordType
	LabelsRecordType
)

type DB struct {
	sync.Mutex
	labels        map[int64][]string
	labelsChanged map[int64]bool
	haveMsgID     map[int64]bool
	fd            *os.File
}

type Header struct {
	Type   uint8
	Length uint32
}

type MessageRecord struct {
	MsgID int64
	Data  []byte
}

type LabelsRecord []LabelsEntry

type LabelsEntry struct {
	MsgID  int64
	Labels []string
}

var magic = []byte("gmls")

func (db *DB) readHeader() (*Header, error) {
	var hdr Header
	err := binary.Read(db.fd, binary.LittleEndian, &hdr)
	if err != nil {
		return nil, err
	}
	return &hdr, nil
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

	if stat, err := f.Stat(); err == nil && stat.Size() == 0 {
		// New file, write magic
		f.Write(magic)
	} else {
		var magicBuf = make([]byte, len(magic))
		f.Read(magicBuf)
		if bytes.Compare(magicBuf, magic) != 0 {
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
			db.haveMsgID[trec.MsgID] = true
		case LabelsRecord:
			for _, lrec := range trec {
				db.labels[lrec.MsgID] = lrec.Labels
			}
		}
	}

	db.Rewind()
	return &db, nil
}

func (db *DB) Rewind() {
	db.fd.Seek(int64(len(magic)), os.SEEK_SET)
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
	rec := MessageRecord{MsgID: msgid, Data: data}
	bs, err := asn1.Marshal(rec)
	if err != nil {
		panic(err)
	}

	defer db.Unlock()
	db.Lock()

	return db.writeRecord(MessageRecordType, bs)
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
		rec := LabelsEntry{MsgID: msgid, Labels: db.labels[msgid]}
		lbls = append(lbls, rec)
	}
	db.labelsChanged = make(map[int64]bool)

	bs, err := asn1.Marshal(lbls)
	if err != nil {
		panic(err)
	}

	return db.writeRecord(LabelsRecordType, bs)
}

func (db *DB) nextRecord(recordType uint8) (interface{}, error) {
	for {
		hdr, err := db.readHeader()
		if err != nil {
			return nil, err
		}

		if recordType != AnyType && hdr.Type != recordType {
			db.fd.Seek(int64(hdr.Length), os.SEEK_CUR)
			continue
		}

		var buf = make([]byte, hdr.Length)
		_, err = db.fd.Read(buf)
		if err != nil {
			return nil, err
		}

		buf = checkDecompress(buf)

		switch hdr.Type {
		case MessageRecordType:
			var msg MessageRecord
			_, err := asn1.Unmarshal(buf, &msg)
			if err != nil {
				panic(err)
			}
			return msg, err

		case LabelsRecordType:
			var lbl LabelsRecord
			_, err := asn1.Unmarshal(buf, &lbl)
			if err != nil {
				panic(err)
			}
			return lbl, err
		}
	}
}

func (db *DB) writeRecord(rtype uint8, data []byte) error {
	pkt := compressedHashed(data)
	hdr := Header{rtype, uint32(len(pkt))}

	db.fd.Seek(0, os.SEEK_END)
	binary.Write(db.fd, binary.LittleEndian, hdr)
	db.fd.Write(pkt)
	return db.fd.Sync()
}

func compressedHashed(bs []byte) []byte {
	ha := sha1.New()
	ha.Write(bs)
	data := ha.Sum(nil)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(bs)
	gz.Close()

	data = append(data, buf.Bytes()...)

	return data
}

func checkDecompress(bs []byte) []byte {
	gz, err := gzip.NewReader(bytes.NewBuffer(bs[20:]))
	if err != nil {
		panic(err)
	}

	data, err := ioutil.ReadAll(gz)
	if err != nil {
		panic(err)
	}

	ha := sha1.New()
	ha.Write(data)
	hash := ha.Sum(nil)

	if bytes.Compare(hash, bs[:20]) != 0 {
		log.Fatalf("Hash mismatch %x != %x", hash, bs[:20])
	}

	return data
}

/*
func (db *DB) Validate() (int, error) {
	var nvalidated int

	for {
		rec, err := db.Read()
		if rec == nil && err == nil {
			break
		}
		if err != nil {
			return nvalidated, err
		}

		gz, _ := gzip.NewReader(bytes.NewBuffer(rec.Data))
		uncompressed, _ := ioutil.ReadAll(gz)
		gz.Close()

		ha := sha1.New()
		ha.Write(uncompressed)
		hash := ha.Sum(nil)

		if bytes.Compare(hash, rec.Hash) != 0 {
			return nvalidated, errors.New("Hash mismatch")
		}

		nvalidated++
	}
	return nvalidated, nil
}
*/
