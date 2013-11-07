package imap

import (
	"crypto/tls"
	"fmt"
	"strconv"
	"time"

	"code.google.com/p/go-imap/go1/imap"
)

type IMAPClient struct {
	imap.Client
}

type MsgID struct {
	UID    uint32
	MsgID  int64
	Labels []string
}

func Client(email, password, mailbox string) (*IMAPClient, error) {
	tlsCfg := tls.Config{
		InsecureSkipVerify: true,
	}

	cl, err := imap.DialTLS("imap.gmail.com:993", &tlsCfg)
	if err != nil {
		return nil, err
	}

	_, err = cl.Login(email, password)
	if err != nil {
		return nil, err
	}

	_, err = cl.Select(mailbox, true)
	if err != nil {
		return nil, err
	}

	go func() {
		// Discard unilateral server data now and then
		time.Sleep(1 * time.Second)
		cl.Data = nil
	}()

	return &IMAPClient{*cl}, nil
}

func (client *IMAPClient) GetMail(uid uint32) ([]byte, error) {
	var set = &imap.SeqSet{}
	set.AddNum(uid)

	cmd, err := client.UIDFetch(set, "RFC822")
	if err != nil {
		return nil, err
	}

	for cmd.InProgress() {
		err = client.Recv(-1)
		if err != nil {
			return nil, err
		}
	}

	resp := cmd.Data[0]
	body := imap.AsBytes(resp.MessageInfo().Attrs["RFC822"])

	return body, nil
}

func (client *IMAPClient) Mailboxes() []string {
	cmd, err := imap.Wait(client.Client.List("", "*"))
	if err != nil {
		return nil
	}

	var res []string
	for _, rsp := range cmd.Data {
		res = append(res, rsp.MailboxInfo().Name)
	}

	return res
}

func (client *IMAPClient) MsgIDSearch(first, last uint32) ([]MsgID, error) {
	ss := fmt.Sprintf("%d:%d", first, last)
	seq, _ := imap.NewSeqSet(ss)
	cmd, err := imap.Wait(client.Client.Fetch(seq, "UID", "X-GM-MSGID", "X-GM-LABELS"))
	if err != nil {
		return nil, err
	}

	var res []MsgID
	for _, rsp := range cmd.Data {
		uid := rsp.MessageInfo().UID
		msgid, _ := strconv.Atoi(rsp.MessageInfo().Attrs["X-GM-MSGID"].(string))
		var labels []string
		for _, lbl := range rsp.MessageInfo().Attrs["X-GM-LABELS"].([]imap.Field) {
			labels = append(labels, lbl.(string))
		}
		res = append(res, MsgID{uid, int64(msgid), labels})
	}
	return res, nil
}
