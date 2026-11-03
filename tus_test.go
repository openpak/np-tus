package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"openpak/np-common/coreclient"
	"openpak/np-common/fakecore"
	"openpak/np-common/identity"
	"openpak/np-common/resource"
	"openpak/np-common/ticket"
	"openpak/np-tus/internal/fakestorage"
)

const commid = "NPWR01234_00"

func TestTUS(t *testing.T) {
	ctx := context.Background()
	const coreKey = "test-core-key-at-least-thirty-two-bytes"
	const alice, bob, carol = "0b7c3f4e-1d2a-4c5b-9e8f-112233445566", "1b7c3f4e-1d2a-4c5b-9e8f-112233445566", "2b7c3f4e-1d2a-4c5b-9e8f-112233445566"
	fake := fakecore.New(coreKey)
	fake.AddAccount(fakecore.Account{ID: alice, DisplayName: "Alice", Country: "PT"})
	fake.AddAccount(fakecore.Account{ID: bob, DisplayName: "Bobby", Country: "US"})
	fake.AddAccount(fakecore.Account{ID: carol, DisplayName: "Carol", Country: "US"})
	fake.AddFriend(alice, bob)
	fake.SetBlocked(carol, alice, true)
	// The storage service asks the same account core; its fake mirrors the friend graph and the block.
	const storageKey = "test-storage-key"
	fs := fakestorage.New(storageKey)
	fs.AddFriend(alice, bob)
	fs.SetBlocked(alice, carol)
	core, err := coreclient.Dial("passthrough:///core", coreKey, fake.Serve(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	links := map[string]*coreclient.Link{}
	for _, id := range []string{alice, bob, carol} {
		acct, err := core.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if links[id], err = identity.EnsureNPLink(ctx, core, acct); err != nil {
			t.Fatal(err)
		}
	}
	storage, conn, err := DialStorage("passthrough:///storage", storageKey, fs.Serve(t)...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ticket.New(key, nil)
	raw, err := signer.Issue(ticket.Identity{AccountID: alice, LinkID: links[alice].LinkID, OnlineID: links[alice].OnlineID, Country: "pt", Service: "IV0001-NPXS01001_00"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tss := t.TempDir()
	os.MkdirAll(filepath.Join(tss, commid), 0o755)
	os.WriteFile(filepath.Join(tss, commid, commid+"-0.tss"), []byte("tss-bytes"), 0o644)
	resourceKey := bytes.Repeat([]byte{7}, 20)
	mux := http.NewServeMux()
	(&Service{Core: core, Tickets: signer, Storage: storage, ResourceKey: resourceKey, StorageFQDN: "tus.example", TSSDir: tss, Log: slog.Default()}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	jid := func(id, cc string) string { return links[id].OnlineID + "@" + cc + ".np.community.playstation.net" }
	env := `<?xml version="1.0" encoding="utf-8"?><npstorage platform="ps3" sv="4.93"><ticket>` + base64.StdEncoding.EncodeToString(raw) +
		`</ticket><ticketjid>` + jid(alice, "pt") + `</ticketjid><psid>00</psid><npcommid version="0">` + commid + `</npcommid><sign pf="ps3">AA==</sign>`
	u := func(id string) string { return `<user mode="0"><jid>` + jid(id, "us") + `</jid></user>` }
	post := func(op, inner string, data []byte) (string, http.Header) {
		t.Helper()
		body := []byte(env + inner + `</npstorage>`)
		req, _ := http.NewRequest("POST", srv.URL+"/user_storage/sec/"+op, nil)
		if data != nil {
			req.Header.Set("Sce-Np-Info-Length", strconv.Itoa(len(body)))
			body = append(append(body, "\r\n\r\n"...), data...)
		}
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Length") == "" {
			t.Fatalf("%s: HTTP %d without Content-Length", op, resp.StatusCode)
		}
		return string(b), resp.Header
	}
	expect := func(got, want string) {
		t.Helper()
		if !strings.Contains(got, want) {
			t.Fatalf("want %q in\n%s", want, got)
		}
	}

	// Discovery is signed with the storage resource key and names the TUS host.
	resp, _ := http.Get(srv.URL + "/np/resource/title/" + commid + "/storage/" + commid + "-storage.xml")
	b, _ := io.ReadAll(resp.Body)
	xmlPart := b[bytes.IndexByte(b, '\n')+1:]
	if !bytes.Equal(b, resource.Sign(xmlPart, resourceKey)) || !bytes.Contains(xmlPart, []byte(`status="alive" titleid="`+commid+`"><storage-fqdn>tus.example<`)) {
		t.Fatalf("discovery: %s", b)
	}

	// Variables on self.
	r, _ := post("set_multi_slot_variable", u(alice)+`<list><variable slot="1" value="100"/><variable slot="2" value="-5"/></list>`, nil)
	expect(r, `result="00"`)
	r, _ = post("get_multi_slot_variable", u(alice)+`<list><variable slot="1"/><variable slot="2"/><variable slot="3"/></list>`, nil)
	expect(r, `<variable slot="1" value="100"><jid>`)
	expect(r, `<variable slot="2" value="-5">`)
	if strings.Contains(r, `slot="3"`) {
		t.Fatal("missing slot returned")
	}
	r, _ = post("addget_variable", u(alice)+`<variable slot="1" value="23"/>`, nil)
	expect(r, `value="123"`)
	r, _ = post("tryset_variable", u(alice)+`<variable slot="1" try="gt" value="50"/>`, nil)
	expect(r, `value="123" old="123"`) // 50 > 123 fails: unchanged, no error
	r, _ = post("tryset_variable", u(alice)+`<variable slot="1" try="gt" value="500"/>`, nil)
	expect(r, `value="500" old="123"`)
	r, _ = post("addget_variable", u(alice)+`<variable slot="1" value="1"/><comp-setter>someone@us.np.community.playstation.net</comp-setter>`, nil)
	expect(r, `result="73"`)

	// Writes to a friend are allowed, to a user who blocked us are not; reads of anyone are.
	r, _ = post("set_multi_slot_variable", u(bob)+`<list><variable slot="1" value="7"/></list>`, nil)
	expect(r, `result="00"`)
	r, _ = post("set_multi_slot_variable", u(carol)+`<list><variable slot="1" value="7"/></list>`, nil)
	expect(r, `result="06"`)
	r, _ = post("get_multi_user_variable", `<variable slot="1"/><user mode="0"><jid>`+jid(alice, "pt")+`</jid><jid>`+jid(bob, "us")+`</jid><jid>`+jid(carol, "us")+`</jid></user>`, nil)
	expect(r, `value="500"`)
	expect(r, `value="7"`)
	r, _ = post("get_multi_slot_variable", `<user mode="0"><jid>nobody@us.np.community.playstation.net</jid></user><list><variable slot="1"/></list>`, nil)
	expect(r, `result="05"`)

	// Friends sorted by value, self included.
	r, _ = post("get_friend_user_variable", `<variable slot="1"/><option offset="0" limit="10" sort="variable" order="desc" owner="true"/>`, nil)
	expect(r, `<hits>2</hits><list><variable slot="1" value="500">`)
	r, _ = post("get_friend_user_variable", `<variable slot="1"/><option offset="0" limit="1" sort="variable" order="asc" owner="true"/>`, nil)
	expect(r, `<hits>2</hits><list><variable slot="1" value="7">`)
	if strings.Count(r, "<variable") != 1 {
		t.Fatal("limit ignored", r)
	}

	// Virtual users.
	r, _ = post("set_multi_slot_variable", `<user mode="1"><online-id>WorldBoss</online-id></user><list><variable slot="4" value="9"/></list>`, nil)
	expect(r, `result="00"`)
	r, _ = post("get_multi_slot_variable", `<user mode="1"><online-id>WorldBoss</online-id></user><list><variable slot="4"/></list>`, nil)
	expect(r, `<online-id>WorldBoss</online-id>`)

	// Data: XML, CRLFCRLF, raw bytes in both directions.
	payload := []byte("\x00\x01binary\r\n\r\npayload")
	r, _ = post("set_data", u(alice)+`<data slot="3"><info>AAEC</info><size>`+strconv.Itoa(len(payload))+`</size></data>`, payload)
	expect(r, `result="00"`)
	r, h := post("get_data", u(alice)+`<data slot="3"/>`, nil)
	n, _ := strconv.Atoi(h.Get("Sce-Np-Info-Length"))
	if n == 0 || r[n:n+4] != "\r\n\r\n" || r[n+4:] != string(payload) || !strings.Contains(r[:n], `<size>`+strconv.Itoa(len(payload))+`</size><info>AAEC</info>`) {
		t.Fatalf("get_data framing: %q", r)
	}
	r, _ = post("get_multi_slot_data_status", u(alice)+`<list><data slot="3"/><data slot="9"/></list>`, nil)
	expect(r, `<data slot="3"><jid>`)
	r, _ = post("get_friend_user_data_status", `<data slot="3"/><option offset="0" limit="10" sort="date" order="desc" owner="true"/>`, nil)
	expect(r, `<hits>1</hits>`)
	r, _ = post("delete_multi_slot_data", u(alice)+`<list><data slot="3"/></list>`, nil)
	expect(r, `result="00"`)
	r, h = post("get_data", u(alice)+`<data slot="3"/>`, nil)
	if expect(r, `result="4a"`); h.Get("Sce-Np-Info-Length") != "" {
		t.Fatal("error reply carries Info-Length")
	}
	r, _ = post("delete_multi_slot_variable", u(alice)+`<list><variable slot="1"/></list>`, nil)
	expect(r, `result="00"`)

	// comp-date: the date the server printed matches; any other does not.
	r, _ = post("get_multi_slot_variable", u(alice)+`<list><variable slot="2"/></list>`, nil)
	i := strings.Index(r, "<date>")
	date := r[i+6 : i+6+strings.Index(r[i+6:], "<")]
	r, _ = post("addget_variable", u(alice)+`<variable slot="2" value="1"/><comp-date>2001-01-01T00:00:00.00Z</comp-date>`, nil)
	expect(r, `result="73"`)
	r, _ = post("addget_variable", u(alice)+`<variable slot="2" value="1"/><comp-date>`+date+`</comp-date>`, nil)
	expect(r, `value="-4"`)

	// The storage service being down is a TUS internal error, not a hang or a wrong answer.
	fs.Down = true
	r, _ = post("get_multi_slot_variable", u(alice)+`<list><variable slot="2"/></list>`, nil)
	expect(r, `result="07"`)
	fs.Down = false

	// Bad ticket.
	env = strings.Replace(env, base64.StdEncoding.EncodeToString(raw), "AAAA", 1)
	r, _ = post("get_multi_slot_variable", u(alice)+`<list><variable slot="2"/></list>`, nil)
	expect(r, `result="02"`)

	// TSS files.
	for path, want := range map[string]int{commid + "/" + commid + "-0.tss": 200, commid + "/" + commid + "-1.tss": 404, commid + "/../x.tss": 404} {
		resp, _ := http.Get(srv.URL + "/tss/np/" + path)
		if resp.StatusCode != want {
			t.Fatalf("tss %s: %d", path, resp.StatusCode)
		}
	}
}
