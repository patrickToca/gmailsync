package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"flag"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calmh/gmailsync/db"
	"github.com/calmh/gmailsync/imap"
	"github.com/calmh/ini"
)

var (
	configFile string = "/etc/gmailsync.ini"
	traceImap  bool
)

var progress struct {
	sync.Mutex
	toScan  int
	scanned int
	toFetch int
	fetched int
	labels  int
}

type MsgID struct {
	UID   uint32
	MsgID int64
}

func main() {
	flag.StringVar(&configFile, "cfg", configFile, "Location of configuration")
	flag.BoolVar(&traceImap, "trace-imap", traceImap, "Verbose trace IMAP operations")
	flag.Parse()
	operation := flag.Arg(0)

	f, err := os.Open(configFile)
	if err != nil {
		log.Fatal(err)
	}
	cfg := ini.Parse(f)
	f.Close()

	switch operation {
	case "list":
		email := cfg.Get("gmail", "email")
		password := cfg.Get("gmail", "password")
		mailbox := cfg.Get("gmail", "folder")
		cl, _ := imap.Client(email, password, mailbox)
		mailboxes := cl.Mailboxes()
		for _, mb := range mailboxes {
			log.Println(mb)
		}

	case "fetch":
		log.Println("Scanning database")
		db, err := db.Open(cfg.Get("gmail", "vault"))
		if err != nil {
			log.Fatal(err)
		}

		log.Printf("Have %d messages", db.Size())

		maxConnections := 4
		if s := cfg.Get("gmail", "connections"); s != "" {
			v, err := strconv.Atoi(s)
			if err == nil {
				maxConnections = v
			}
		}
		if maxConnections < 2 {
			maxConnections = 2
			log.Println("Minimum number of connections is 2")
		}

		uids := findNewUIDs(cfg, db)

		var wg sync.WaitGroup
		for i := 1; i < maxConnections; i++ {
			wg.Add(1)
			go fetchAndStore(cfg, i, db, uids, &wg)
		}

		go func() {
			for {
				time.Sleep(10 * time.Second)
				progress.Lock()
				log.Printf("%d of %d scanned, %d of %d fetched, %d labelupdated", progress.scanned, progress.toScan, progress.fetched, progress.toFetch, progress.labels)
				progress.Unlock()
			}
		}()

		wg.Wait()
		/*
			case "validate":
				log.Println("Performing validation")
				db, err := OpenRead(cfg.Get("gmail", "vault"))
				if err != nil {
					log.Fatal(err)
				}

				n, err := db.Validate()
				if err != nil {
					log.Fatal(err)
				}
				log.Printf("Validated %d messages", n)
		*/

	case "mbox":
		db, err := db.Open(cfg.Get("gmail", "vault"))
		if err != nil {
			log.Fatal(err)
		}

		mbox(db, os.Stdout)
	}
}

func findNewUIDs(cfg ini.Config, db *db.DB) chan MsgID {
	if traceImap {
		log.Printf("IMAP[0]: Connect")
	}

	email := cfg.Get("gmail", "email")
	password := cfg.Get("gmail", "password")
	mailbox := cfg.Get("gmail", "folder")
	client, err := imap.Client(email, password, mailbox)
	if err != nil {
		log.Fatal(err)
	}

	if traceImap {
		log.Printf("IMAP[0]: %d messages to scan", client.Mailbox.Messages)
	}
	progress.Lock()
	progress.toScan = int(client.Mailbox.Messages)
	progress.Unlock()

	out := make(chan MsgID, client.Mailbox.Messages)

	go func() {
		step := uint32(1000)
		for m := uint32(1); m <= client.Mailbox.Messages+step; m += step {
			if traceImap {
				log.Printf("IMAP[0]: UID SEARCH %d:%d", m, m+step-1)
			}

			msgids, err := client.MsgIDSearch(m, m+step-1)
			if err != nil {
				log.Fatal(err)
			}
			progress.Lock()
			progress.scanned += len(msgids)
			progress.Unlock()
			for _, msgid := range msgids {
				if !db.HaveUID(msgid.MsgID) {
					out <- MsgID{msgid.UID, msgid.MsgID}
					progress.Lock()
					progress.toFetch++
					progress.Unlock()
				}

				if !sliceEquals(msgid.Labels, db.Labels(msgid.MsgID)) {
					db.SetLabels(msgid.MsgID, msgid.Labels)
					progress.Lock()
					progress.labels++
					progress.Unlock()
				}
			}

			err = db.WriteLabels()
			if err != nil {
				log.Fatal(err)
			}
		}
		close(out)
	}()

	return out
}

func sliceEquals(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fetchAndStore(cfg ini.Config, id int, db *db.DB, msgids chan MsgID, wg *sync.WaitGroup) {
	if traceImap {
		log.Printf("IMAP[%d]: Connect", id)
	}

	email := cfg.Get("gmail", "email")
	password := cfg.Get("gmail", "password")
	mailbox := cfg.Get("gmail", "folder")
	client, err := imap.Client(email, password, mailbox)
	if err != nil {
		log.Fatal(err)
	}

	if traceImap {
		log.Printf("IMAP[%d]: Ready", id)
	}

	for msgid := range msgids {
		if traceImap {
			log.Printf("IMAP[%d]: UID FETCH %d", id, msgid.MsgID)
		}

		body, err := client.GetMail(msgid.UID)
		if err != nil {
			log.Fatal(err)
		}

		err = db.WriteMessage(msgid.MsgID, body)
		if err != nil {
			log.Fatal(err)
		}

		progress.Lock()
		progress.fetched++
		progress.Unlock()
	}

	wg.Done()
}

func mbox(db *db.DB, wr io.Writer) {
	var nwritten int
	nl := []byte("\n")
	from := []byte("From ")
	esc := []byte(">")

	bwr := bufio.NewWriter(wr)

	for {
		rec, err := db.ReadMessage()
		if err == io.EOF {
			break
		}

		gz, _ := gzip.NewReader(bytes.NewBuffer(rec.Data))
		bwr.Write([]byte("From MAILER-DAEMON Thu Jan  1 01:00:00 1970\n"))
		if labels := db.Labels(rec.MsgID); len(labels) > 0 {
			bwr.Write([]byte("X-Gmail-Labels: " + strings.Join(labels, ", ") + "\n"))
		}
		bwr.Write([]byte("X-Gmail-MsgID: " + strconv.FormatInt(rec.MsgID, 10) + "\n"))
		s := bufio.NewScanner(gz)
		for s.Scan() {
			line := s.Bytes()
			if bytes.HasPrefix(line, from) {
				bwr.Write(esc)
			}
			bwr.Write(line)
			bwr.Write(nl)
		}
		bwr.Write(nl)
		bwr.Flush()

		gz.Close()
		nwritten++
	}

	log.Printf("Wrote %d messages to stdout", nwritten)
}
