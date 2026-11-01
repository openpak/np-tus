package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
	"openpak/np-common/coreclient"
	"openpak/np-common/onlineid"
	"openpak/np-common/resource"
	"openpak/np-common/ticket"
)

// Wire format: research notes "tus-tss-protocol" (firmware 4.93 VSH). Replies are always HTTP 200
// with an explicit Content-Length; errors are the root result attribute (2 hex digits).
const (
	resultOK          = "00"
	resultBadTicket   = "02"
	resultBadNpID     = "05"
	resultForbidden   = "06"
	resultInternal    = "07"
	resultTooLarge    = "1a"
	resultBadVirtual  = "49"
	resultNoData      = "4a"
	resultCondition   = "73"
	resultUnspecified = "ff"

	maxXML   = 128 << 10
	maxData  = 1 << 20 // ponytail: per-slot cap; Sony's server limit is unknown
	maxSlots = 256
	dateFmt  = "2006-01-02T15:04:05.00Z"
)

var commIDPattern = regexp.MustCompile(`^[A-Z]{4}[0-9]{5}_[0-9]{2}$`)

type Core interface {
	ticket.AccountCore
	GetLinkByOnlineID(context.Context, string) (*coreclient.Link, error)
	Friends(context.Context, string) ([]*accountv1.Friend, error)
	CanInteract(context.Context, string, string) (bool, error)
}

type Service struct {
	Core        Core
	Tickets     *ticket.Signer
	Store       *Store
	ResourceKey []byte // storage resource key, firmware key slot 6
	StorageFQDN string
	TSSDir      string
	Log         *slog.Logger
}

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /np/resource/title/{commid}/storage/{file}", s.discovery)
	mux.HandleFunc("GET /tss/{env}/{commid}/{file}", s.tss)
	for op, h := range map[string]func(*call){
		"set_multi_slot_variable":     s.setMultiSlotVariable,
		"get_multi_slot_variable":     s.getMultiSlotVariable,
		"get_multi_user_variable":     s.getMultiUserVariable,
		"get_friend_user_variable":    s.getFriendUserVariable,
		"tryset_variable":             s.trysetVariable,
		"addget_variable":             s.addgetVariable,
		"delete_multi_slot_variable":  s.deleteMultiSlotVariable,
		"set_data":                    s.setData,
		"get_data":                    s.getData,
		"get_multi_slot_data_status":  s.getMultiSlotDataStatus,
		"get_multi_user_data_status":  s.getMultiUserDataStatus,
		"get_friend_user_data_status": s.getFriendUserDataStatus,
		"delete_multi_slot_data":      s.deleteMultiSlotData,
	} {
		mux.HandleFunc("POST /user_storage/sec/"+op, s.handle(op, h))
	}
}

// discovery is the signed storage resource VSH reads before any TUS call.
func (s *Service) discovery(w http.ResponseWriter, r *http.Request) {
	commid := r.PathValue("commid")
	if !commIDPattern.MatchString(commid) || r.PathValue("file") != commid+"-storage.xml" {
		http.NotFound(w, r)
		return
	}
	body := resource.Sign([]byte(`<?xml version="1.0" encoding="utf-8"?><npstorage status="alive" titleid="`+commid+
		`"><storage-fqdn>`+s.StorageFQDN+`</storage-fqdn></npstorage>`), s.ResourceKey)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// tss serves title small storage files from TSSDir/<commid>/<commid>-<slot>.tss. A missing file is
// a 404, which VSH reports to the game as zero bytes.
func (s *Service) tss(w http.ResponseWriter, r *http.Request) {
	commid, file := r.PathValue("commid"), r.PathValue("file")
	if s.TSSDir == "" || !commIDPattern.MatchString(commid) || !regexp.MustCompile(`^`+commid+`-[0-9]+\.tss$`).MatchString(file) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.TSSDir, commid, file))
}

type user struct {
	Mode      string   `xml:"mode,attr"`
	JIDs      []string `xml:"jid"`
	OnlineIDs []string `xml:"online-id"`
}
type slotIn struct {
	Slot      int    `xml:"slot,attr"`
	Value     string `xml:"value,attr"`
	Try       string `xml:"try,attr"`
	CompValue string `xml:"comp-value,attr"`
	Info      string `xml:"info"`
	Size      int64  `xml:"size"`
}
type request struct {
	XMLName    xml.Name `xml:"npstorage"`
	Ticket     string   `xml:"ticket"`
	CommID     string   `xml:"npcommid"`
	User       user     `xml:"user"`
	Variable   *slotIn  `xml:"variable"`
	Data       *slotIn  `xml:"data"`
	List       struct {
		Variables []slotIn `xml:"variable"`
		Data      []slotIn `xml:"data"`
	} `xml:"list"`
	Option struct {
		Offset int    `xml:"offset,attr"`
		Limit  int    `xml:"limit,attr"`
		Sort   string `xml:"sort,attr"`
		Order  string `xml:"order,attr"`
		Owner  string `xml:"owner,attr"`
	} `xml:"option"`
	CompDate   string `xml:"comp-date"`
	CompSetter string `xml:"comp-setter"`
}

// owner is one storage owner: a core account, or a title's virtual user.
type owner struct {
	key      string // account UUID or "v:<name>"
	jid      string // reply identity, echoed from the request
	virtual  bool
	account  string
	online   string
	updated  time.Time // set by friend queries for sorting
	sortable int64
}

type call struct {
	w        http.ResponseWriter
	r        *http.Request
	req      request
	actor    ticket.Identity
	actorJID string
	data     []byte // raw payload of set_data
}

func (s *Service) handle(op string, h func(*call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &call{w: w, r: r}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxXML+maxData+4))
		if err != nil {
			reply(w, resultTooLarge, nil)
			return
		}
		if n, err := strconv.Atoi(r.Header.Get("Sce-Np-Info-Length")); err == nil && op == "set_data" {
			if n < 0 || n+4 > len(body) || !bytes.Equal(body[n:n+4], []byte("\r\n\r\n")) {
				reply(w, resultUnspecified, nil)
				return
			}
			body, c.data = body[:n], body[n+4:]
		}
		if len(body) > maxXML || xml.Unmarshal(body, &c.req) != nil || !commIDPattern.MatchString(c.req.CommID) {
			reply(w, resultUnspecified, nil)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(c.req.Ticket), ""))
		if err == nil {
			c.actor, err = ticket.VerifyAccount(r.Context(), s.Core, s.Tickets, raw)
		}
		if err != nil {
			reply(w, resultBadTicket, nil)
			return
		}
		c.actorJID = s.jid(r.Context(), c.actor.AccountID, c.actor.OnlineID)
		h(c)
	}
}

func (s *Service) jid(ctx context.Context, account, online string) string {
	country := "us"
	if a, err := s.Core.GetAccount(ctx, account); err == nil && len(a.Country) == 2 {
		country = strings.ToLower(a.Country)
	}
	return online + "@" + country + ".np.community.playstation.net"
}

// owners resolves the request's <user>; the result code is "" on success.
func (s *Service) owners(c *call) ([]owner, string) {
	u := c.req.User
	var out []owner
	if u.Mode == "1" {
		for _, name := range u.OnlineIDs {
			if !onlineid.Valid(name) {
				return nil, resultBadVirtual
			}
			out = append(out, owner{key: "v:" + strings.ToLower(name), jid: name, virtual: true})
		}
	} else {
		for _, jid := range u.JIDs {
			online, _, _ := strings.Cut(jid, "@")
			link, err := s.Core.GetLinkByOnlineID(c.r.Context(), online)
			if err != nil {
				if errors.Is(err, coreclient.ErrUnavailable) {
					return nil, resultInternal
				}
				return nil, resultBadNpID
			}
			out = append(out, owner{key: link.AccountID, jid: jid, account: link.AccountID, online: link.OnlineID})
		}
	}
	if len(out) == 0 || len(out) > maxSlots {
		return nil, resultUnspecified
	}
	return out, ""
}

// writable checks that the actor may write to the owner: itself, a virtual user, or a real user
// it may interact with (no block either way).
func (s *Service) writable(c *call, o owner) string {
	if o.virtual || o.account == c.actor.AccountID {
		return ""
	}
	ok, err := s.Core.CanInteract(c.r.Context(), c.actor.AccountID, o.account)
	if err != nil {
		return resultInternal
	}
	if !ok {
		return resultForbidden
	}
	return ""
}

// single resolves exactly one target, checking write access when write is set.
func (s *Service) single(c *call, write bool) (owner, bool) {
	targets, code := s.owners(c)
	if code == "" && len(targets) != 1 {
		code = resultUnspecified
	}
	if code == "" && write {
		code = s.writable(c, targets[0])
	}
	if code != "" {
		reply(c.w, code, nil)
		return owner{}, false
	}
	return targets[0], true
}

func slots(items []slotIn) []int {
	out := make([]int, 0, len(items))
	for _, it := range items {
		out = append(out, it.Slot)
	}
	return out
}

func (s *Service) fail(c *call, err error) {
	s.Log.Warn("tus store", "err", err)
	reply(c.w, resultInternal, nil)
}

func (s *Service) setMultiSlotVariable(c *call) {
	o, ok := s.single(c, true)
	if !ok {
		return
	}
	var vars []Variable
	for _, it := range c.req.List.Variables {
		v, err := strconv.ParseInt(it.Value, 10, 64)
		if err != nil {
			reply(c.w, resultUnspecified, nil)
			return
		}
		vars = append(vars, Variable{Slot: it.Slot, Value: v})
	}
	if len(vars) == 0 || len(vars) > maxSlots {
		reply(c.w, resultUnspecified, nil)
		return
	}
	if err := s.Store.SetVariables(c.r.Context(), c.req.CommID, o.key, c.actorJID, vars); err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, nil)
}

func (s *Service) getMultiSlotVariable(c *call) {
	o, ok := s.single(c, false)
	if !ok {
		return
	}
	got, err := s.Store.Variables(c.r.Context(), c.req.CommID, o.key, slots(c.req.List.Variables))
	if err != nil {
		s.fail(c, err)
		return
	}
	var items []any
	for _, slot := range slots(c.req.List.Variables) {
		if v, ok := got[slot]; ok {
			items = append(items, variableOut(o, v, nil))
		}
	}
	reply(c.w, resultOK, listOut(-1, items))
}

func (s *Service) getMultiUserVariable(c *call) {
	if c.req.Variable == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	targets, code := s.owners(c)
	if code != "" {
		reply(c.w, code, nil)
		return
	}
	var items []any
	for _, o := range targets {
		got, err := s.Store.Variables(c.r.Context(), c.req.CommID, o.key, []int{c.req.Variable.Slot})
		if err != nil {
			s.fail(c, err)
			return
		}
		if v, ok := got[c.req.Variable.Slot]; ok {
			items = append(items, variableOut(o, v, nil))
		}
	}
	reply(c.w, resultOK, listOut(-1, items))
}

// friends returns the actor's friends (plus the actor when option owner="true") as owners.
func (s *Service) friends(c *call) ([]owner, bool) {
	fs, err := s.Core.Friends(c.r.Context(), c.actor.AccountID)
	if err != nil {
		reply(c.w, resultInternal, nil)
		return nil, false
	}
	var out []owner
	if c.req.Option.Owner == "true" {
		out = append(out, owner{key: c.actor.AccountID, jid: c.actorJID, account: c.actor.AccountID, online: c.actor.OnlineID})
	}
	for _, f := range fs {
		link, err := s.Core.GetLinkByAccount(c.r.Context(), f.AccountId)
		if err != nil {
			continue // a friend without an NP link has no PS3 storage
		}
		out = append(out, owner{key: f.AccountId, jid: s.jid(c.r.Context(), f.AccountId, link.OnlineID), account: f.AccountId, online: link.OnlineID})
	}
	return out, true
}

// page sorts and slices friend results: sort "date" or "variable", order "asc" or "desc".
func page(c *call, found []owner, items []any) (int, []any) {
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := found[idx[a]], found[idx[b]]
		if c.req.Option.Order == "asc" {
			x, y = y, x
		}
		if c.req.Option.Sort == "variable" {
			return x.sortable > y.sortable
		}
		return x.updated.After(y.updated)
	})
	limit, offset := c.req.Option.Limit, max(c.req.Option.Offset, 0)
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var out []any
	for i := offset; i < len(idx) && len(out) < limit; i++ {
		out = append(out, items[idx[i]])
	}
	return len(items), out
}

func (s *Service) getFriendUserVariable(c *call) {
	if c.req.Variable == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	fs, ok := s.friends(c)
	if !ok {
		return
	}
	var found []owner
	var items []any
	for _, o := range fs {
		got, err := s.Store.Variables(c.r.Context(), c.req.CommID, o.key, []int{c.req.Variable.Slot})
		if err != nil {
			s.fail(c, err)
			return
		}
		if v, ok := got[c.req.Variable.Slot]; ok {
			o.updated, o.sortable = v.Updated, v.Value
			found = append(found, o)
			items = append(items, variableOut(o, v, nil))
		}
	}
	hits, items := page(c, found, items)
	reply(c.w, resultOK, listOut(hits, items))
}

// conditions checks comp-date / comp-setter against the slot's last change.
func conditions(c *call, updated *time.Time, setter string) bool {
	if c.req.CompDate != "" && (updated == nil || updated.UTC().Format(dateFmt) != c.req.CompDate) {
		return false
	}
	return c.req.CompSetter == "" || (updated != nil && c.req.CompSetter == setter)
}

// trysetVariable stores value when "value <try> current" holds (missing counts as 0); comp-value,
// when given, replaces value as the compared operand. A failed comparison is not an error: the
// reply carries the unchanged value.
// ponytail: operand order is inferred from the high-score idiom; confirm against a title.
func (s *Service) trysetVariable(c *call) {
	it := c.req.Variable
	if it == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	value, err := strconv.ParseInt(it.Value, 10, 64)
	operand := value
	if err == nil && it.CompValue != "" {
		operand, err = strconv.ParseInt(it.CompValue, 10, 64)
	}
	if err != nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	o, ok := s.single(c, true)
	if !ok {
		return
	}
	failed := false
	var old int64
	next, err := s.Store.Update(c.r.Context(), c.req.CommID, o.key, c.actorJID, it.Slot, func(cur *Variable) (int64, bool) {
		var current int64
		var when *time.Time
		setter := ""
		if cur != nil {
			current, when, setter = cur.Value, &cur.Updated, cur.Setter
		}
		old = current
		if !conditions(c, when, setter) {
			failed = true
			return 0, false
		}
		return value, compare(it.Try, operand, current)
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	if failed {
		reply(c.w, resultCondition, nil)
		return
	}
	if next == nil {
		next = &Variable{Slot: it.Slot, Setter: c.actorJID, Updated: time.Now()}
	}
	reply(c.w, resultOK, variableOut(o, *next, &old))
}

func compare(op string, a, b int64) bool {
	switch op {
	case "eq":
		return a == b
	case "ne":
		return a != b
	case "gt":
		return a > b
	case "ge":
		return a >= b
	case "lt":
		return a < b
	case "le":
		return a <= b
	}
	return false
}

func (s *Service) addgetVariable(c *call) {
	it := c.req.Variable
	if it == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	delta, err := strconv.ParseInt(it.Value, 10, 64)
	if err != nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	o, ok := s.single(c, true)
	if !ok {
		return
	}
	if c.req.CompDate != "" || c.req.CompSetter != "" {
		failed := false
		next, err := s.Store.Update(c.r.Context(), c.req.CommID, o.key, c.actorJID, it.Slot, func(cur *Variable) (int64, bool) {
			var current int64
			var when *time.Time
			setter := ""
			if cur != nil {
				current, when, setter = cur.Value, &cur.Updated, cur.Setter
			}
			if !conditions(c, when, setter) {
				failed = true
				return 0, false
			}
			return current + delta, true
		})
		if err != nil {
			s.fail(c, err)
		} else if failed {
			reply(c.w, resultCondition, nil)
		} else {
			reply(c.w, resultOK, variableOut(o, *next, nil))
		}
		return
	}
	v, err := s.Store.AddVariable(c.r.Context(), c.req.CommID, o.key, c.actorJID, it.Slot, delta)
	if err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, variableOut(o, v, nil))
}

func (s *Service) deleteMultiSlotVariable(c *call) {
	o, ok := s.single(c, true)
	if !ok {
		return
	}
	if err := s.Store.DeleteVariables(c.r.Context(), c.req.CommID, o.key, slots(c.req.List.Variables)); err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, nil)
}

func (s *Service) setData(c *call) {
	it := c.req.Data
	if it == nil || c.data == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	info, err := base64.StdEncoding.DecodeString(strings.TrimSpace(it.Info))
	if err != nil || len(info) > 384 {
		reply(c.w, resultUnspecified, nil)
		return
	}
	if len(c.data) > maxData {
		reply(c.w, resultTooLarge, nil)
		return
	}
	o, ok := s.single(c, true)
	if !ok {
		return
	}
	if c.req.CompDate != "" || c.req.CompSetter != "" {
		cur, err := s.Store.GetData(c.r.Context(), c.req.CommID, o.key, it.Slot)
		if err != nil {
			s.fail(c, err)
			return
		}
		var when *time.Time
		setter := ""
		if cur != nil {
			when, setter = &cur.Updated, cur.Setter
		}
		if !conditions(c, when, setter) {
			reply(c.w, resultCondition, nil)
			return
		}
	}
	if err := s.Store.SetData(c.r.Context(), c.req.CommID, o.key, c.actorJID, it.Slot, info, c.data); err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, nil)
}

func (s *Service) getData(c *call) {
	if c.req.Data == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	o, ok := s.single(c, false)
	if !ok {
		return
	}
	d, err := s.Store.GetData(c.r.Context(), c.req.CommID, o.key, c.req.Data.Slot)
	if err != nil {
		s.fail(c, err)
		return
	}
	if d == nil {
		reply(c.w, resultNoData, nil)
		return
	}
	x := envelope(resultOK, dataOut(o, *d, false))
	c.w.Header().Set("Sce-Np-Info-Length", strconv.Itoa(len(x)))
	body := append(append(x, "\r\n\r\n"...), d.Body...)
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.Write(body)
}

func (s *Service) getMultiSlotDataStatus(c *call) {
	o, ok := s.single(c, false)
	if !ok {
		return
	}
	got, err := s.Store.DataStatus(c.r.Context(), c.req.CommID, o.key, slots(c.req.List.Data))
	if err != nil {
		s.fail(c, err)
		return
	}
	var items []any
	for _, slot := range slots(c.req.List.Data) {
		if d, ok := got[slot]; ok {
			items = append(items, dataOut(o, d, true))
		}
	}
	reply(c.w, resultOK, listOut(-1, items))
}

func (s *Service) getMultiUserDataStatus(c *call) {
	if c.req.Data == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	targets, code := s.owners(c)
	if code != "" {
		reply(c.w, code, nil)
		return
	}
	var items []any
	for _, o := range targets {
		got, err := s.Store.DataStatus(c.r.Context(), c.req.CommID, o.key, []int{c.req.Data.Slot})
		if err != nil {
			s.fail(c, err)
			return
		}
		if d, ok := got[c.req.Data.Slot]; ok {
			items = append(items, dataOut(o, d, true))
		}
	}
	reply(c.w, resultOK, listOut(-1, items))
}

func (s *Service) getFriendUserDataStatus(c *call) {
	if c.req.Data == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	fs, ok := s.friends(c)
	if !ok {
		return
	}
	var found []owner
	var items []any
	for _, o := range fs {
		got, err := s.Store.DataStatus(c.r.Context(), c.req.CommID, o.key, []int{c.req.Data.Slot})
		if err != nil {
			s.fail(c, err)
			return
		}
		if d, ok := got[c.req.Data.Slot]; ok {
			o.updated, o.sortable = d.Updated, d.Size
			found = append(found, o)
			items = append(items, dataOut(o, d, true))
		}
	}
	hits, items := page(c, found, items)
	reply(c.w, resultOK, listOut(hits, items))
}

func (s *Service) deleteMultiSlotData(c *call) {
	o, ok := s.single(c, true)
	if !ok {
		return
	}
	if err := s.Store.DeleteData(c.r.Context(), c.req.CommID, o.key, slots(c.req.List.Data)); err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, nil)
}

// Reply XML. Owners are named by <jid>, or <online-id> for virtual users.
type ownerXML struct {
	JID      string `xml:"jid,omitempty"`
	OnlineID string `xml:"online-id,omitempty"`
}

func ownerOf(o owner) ownerXML {
	if o.virtual {
		return ownerXML{OnlineID: o.jid}
	}
	return ownerXML{JID: o.jid}
}

type variableXML struct {
	XMLName xml.Name `xml:"variable"`
	Slot    int      `xml:"slot,attr"`
	Value   int64    `xml:"value,attr"`
	Old     *int64   `xml:"old,attr,omitempty"`
	ownerXML
	Date   string `xml:"date"`
	Setter string `xml:"setterjid"`
}

func variableOut(o owner, v Variable, old *int64) variableXML {
	return variableXML{Slot: v.Slot, Value: v.Value, Old: old, ownerXML: ownerOf(o), Date: v.Updated.UTC().Format(dateFmt), Setter: v.Setter}
}

type dataXML struct {
	XMLName xml.Name `xml:"data"`
	Slot    *int     `xml:"slot,attr,omitempty"`
	ownerXML
	Size   int64  `xml:"size"`
	Info   string `xml:"info"`
	Date   string `xml:"date"`
	Setter string `xml:"setterjid"`
}

func dataOut(o owner, d Data, withSlot bool) dataXML {
	x := dataXML{ownerXML: ownerOf(o), Size: d.Size, Info: base64.StdEncoding.EncodeToString(d.Info), Date: d.Updated.UTC().Format(dateFmt), Setter: d.Setter}
	if withSlot {
		x.Slot = &d.Slot
	}
	return x
}

type listXML struct {
	Hits  *int `xml:"hits,omitempty"`
	Items []any
}

// MarshalXML writes [<hits>]<list>…</list>; <list> is present even when empty.
func (l listXML) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	if l.Hits != nil {
		if err := e.EncodeElement(*l.Hits, xml.StartElement{Name: xml.Name{Local: "hits"}}); err != nil {
			return err
		}
	}
	return e.EncodeElement(struct{ Items []any }{l.Items}, xml.StartElement{Name: xml.Name{Local: "list"}})
}

func listOut(hits int, items []any) listXML {
	l := listXML{Items: items}
	if hits >= 0 {
		l.Hits = &hits
	}
	return l
}

func envelope(result string, inner any) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><npstorage result="` + result + `">`)
	if inner != nil {
		xml.NewEncoder(&b).Encode(inner)
	}
	b.WriteString(`</npstorage>`)
	return b.Bytes()
}

func reply(w http.ResponseWriter, result string, inner any) {
	b := envelope(result, inner)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}
