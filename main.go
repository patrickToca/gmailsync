package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
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
	fetched int
	labels  int
}

type MsgID struct {
	UID   uint32
	MsgID int64
}

func main() {
	fs := flag.NewFlagSet("gmailsync", flag.ExitOnError)
	fs.StringVar(&configFile, "cfg", configFile, "Configuration file name")
	fs.BoolVar(&traceImap, "trace-imap", traceImap, "Verbose trace IMAP operations")
	fs.Usage = func() {
		fmt.Println("Usage:")
		fmt.Println("  gmailsync [options] <command>")
		fmt.Println()
		fmt.Println("Command is one of:")
		fmt.Println("  fetch - Fetch new mail from GMail")
		fmt.Println("  mbox  - Write an MBOX file with all messages to stdout")
		fmt.Println("  list  - List available mailboxes")
		fmt.Println()
		fmt.Println("Options (with default values):")
		fs.PrintDefaults()
		fmt.Println()
		fmt.Println("Options, if any, must precede the command.")
		fmt.Println()
	}
	err := fs.Parse(os.Args[1:])
	operation := fs.Arg(0)

	switch operation {
	case "list", "fetch", "mbox":
	default:
		fs.Usage()
		os.Exit(1)
	}

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
		mailbox := cfg.Get("gmail", "mailbox")
		cl, _ := imap.Client(email, password, mailbox)
		mailboxes := cl.Mailboxes()
		for _, mb := range mailboxes {
			fmt.Println(mb)
		}

	case "fetch":
		log.Println("Scanning & validating database")
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
				lock(&progress, func() {
					log.Printf("%d of %d scanned, %d fetched, %d labelupdated", progress.scanned, progress.toScan, progress.fetched, progress.labels)
				})
			}
		}()

		wg.Wait()

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
	mailbox := cfg.Get("gmail", "mailbox")
	client, err := imap.Client(email, password, mailbox)
	if err != nil {
		log.Fatal(err)
	}

	if traceImap {
		log.Printf("IMAP[0]: %d messages in mailbox", client.Mailbox.Messages)
	}
	lock(&progress, func() {
		progress.toScan = int(client.Mailbox.Messages)
	})

	step := uint32(100)
	out := make(chan MsgID, step)

	go func() {
		begin := uint32(1)
		for begin < client.Mailbox.Messages {
			end := begin + step - 1
			if traceImap {
				log.Printf("IMAP[0]: UID SEARCH %d:%d", begin, end)
			}

			msgids, err := client.MsgIDSearch(begin, end)
			if err != nil {
				log.Fatal(err)
			}
			lock(&progress, func() {
				progress.scanned += len(msgids)
			})

			begin += step

			fetch := 0
			for _, msgid := range msgids {
				if !db.HaveUID(msgid.MsgID) {
					out <- MsgID{msgid.UID, msgid.MsgID}
					fetch++
				}

				if !sliceEquals(msgid.Labels, db.Labels(msgid.MsgID)) {
					db.SetLabels(msgid.MsgID, msgid.Labels)
					lock(&progress, func() {
						progress.labels++
					})
				}
			}

			err = db.WriteLabels()
			if err != nil {
				log.Fatal(err)
			}

			if fetch == 0 && step < 3200 {
				// Scale up for faster scanning of known messages
				step *= 2
			} else if fetch > 0 && step > 100 {
				// Scale down to avoid timeouts and write reasonable label
				// chunks when we need to fetch lots of messages.
				step /= 2
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
	mailbox := cfg.Get("gmail", "mailbox")
	client, err := imap.Client(email, password, mailbox)
	if err != nil {
		log.Fatal(err)
	}

	if traceImap {
		log.Printf("IMAP[%d]: Ready", id)
	}

	for {
		select {
		case msgid, ok := <-msgids:
			if !ok {
				break
			}
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

			lock(&progress, func() {
				progress.fetched++
			})
		}
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

		bwr.Write([]byte("From MAILER-DAEMON Thu Jan  1 01:00:00 1970\n"))
		if labels := db.Labels(rec.MessageID); len(labels) > 0 {
			bwr.Write([]byte("X-Gmail-Labels: " + strings.Join(labels, ", ") + "\n"))
		}
		bwr.Write([]byte("X-Gmail-MsgID: " + strconv.FormatInt(rec.MessageID, 10) + "\n"))
		s := bufio.NewScanner(bytes.NewBuffer(rec.Data))
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

		nwritten++
	}

	log.Printf("Wrote %d messages to stdout", nwritten)
}

type Locker interface {
	Lock()
	Unlock()
}

func lock(l Locker, f func()) {
	defer l.Unlock()
	l.Lock()
	f()
}
