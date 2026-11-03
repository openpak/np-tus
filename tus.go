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
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"openpak/np-common/coreclient"
	"openpak/np-common/onlineid"
	"openpak/np-common/resource"
	"openpak/np-common/ticket"
	pb "openpak/np-tus/internal/titlestoragepb"
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
	maxData  = 1 << 20 // ponytail: per-slot cap; Sony's server limit is unknown (the core allows more)
	maxSlots = 256
	dateFmt  = "2006-01-02T15:04:05.00Z"
)

var commIDPattern = regexp.MustCompile(`^[A-Z]{4}[0-9]{5}_[0-9]{2}$`)

// network is the account-core link namespace the title storage files PS3 rows under; dataColl
// is the object collection that holds the data slots (the object name is the decimal slot).
const (
	network  = "np"
	dataColl = "tus"
)

// Core is the account core as np-tus uses it: tickets and the online-id <-> account links. The
// friend graph and the "may this player write there" rule live in the title-storage service.
type Core interface {
	ticket.AccountCore
	GetLinkByOnlineID(context.Context, string) (*coreclient.Link, error)
}

type Service struct {
	Core        Core
	Tickets     *ticket.Signer
	Storage     pb.TitleStorageClient
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
	XMLName  xml.Name `xml:"npstorage"`
	Ticket   string   `xml:"ticket"`
	CommID   string   `xml:"npcommid"`
	User     user     `xml:"user"`
	Variable *slotIn  `xml:"variable"`
	Data     *slotIn  `xml:"data"`
	List     struct {
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
	key     string // account UUID or "v:<name>"
	jid     string // reply identity, echoed from the request
	virtual bool
	account string
	online  string
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

// single resolves exactly one target. Whether the caller may write there is the storage
// service's rule (the account core's interaction check), applied on the write.
func (s *Service) single(c *call) (owner, bool) {
	targets, code := s.owners(c)
	if code == "" && len(targets) != 1 {
		code = resultUnspecified
	}
	if code != "" {
		reply(c.w, code, nil)
		return owner{}, false
	}
	return targets[0], true
}

// ref names the title in the storage service's terms: PS3 link namespace, communication id.
func ref(commid string) *pb.TitleRef { return &pb.TitleRef{Network: network, Title: commid} }

// every row is written public: TUS lets any player read any player's slots, and the storage
// service defaults to private.
const vis = pb.Visibility_VISIBILITY_PUBLIC

// fail maps a storage service error to a TUS result code.
func (s *Service) fail(c *call, err error) {
	switch status.Code(err) {
	case codes.PermissionDenied:
		reply(c.w, resultForbidden, nil)
	case codes.ResourceExhausted:
		reply(c.w, resultTooLarge, nil)
	case codes.InvalidArgument:
		reply(c.w, resultUnspecified, nil)
	default:
		s.Log.Warn("tus storage", "err", err)
		reply(c.w, resultInternal, nil)
	}
}

// condition is the request's comp-date / comp-setter as the storage service's write condition.
// A date that does not parse can never match a stored time.
func (c *call) condition() *pb.Condition {
	if c.req.CompDate == "" && c.req.CompSetter == "" {
		return nil
	}
	out := &pb.Condition{}
	if c.req.CompDate != "" {
		ms := int64(-1)
		if t, err := time.Parse(dateFmt, c.req.CompDate); err == nil {
			ms = t.UnixMilli()
		}
		out.ExpectUpdatedUnixMs = &ms
	}
	if c.req.CompSetter != "" {
		out.ExpectSetter = &c.req.CompSetter
	}
	return out
}

// Variable and Data are the slots as the reply XML renders them.
type Variable struct {
	Slot    int
	Value   int64
	Setter  string
	Updated time.Time
}

type Data struct {
	Slot    int
	Info    []byte
	Size    int64
	Setter  string
	Updated time.Time
}

func variableFrom(v *pb.Variable) Variable {
	return Variable{Slot: int(v.GetSlot()), Value: v.GetValue(), Setter: v.GetSetter(), Updated: time.UnixMilli(v.GetUpdatedUnixMs())}
}

func dataFrom(o *pb.Object) Data {
	slot, _ := strconv.Atoi(o.GetName())
	return Data{Slot: slot, Info: o.GetInfo(), Size: int64(o.GetSize()), Setter: o.GetSetter(), Updated: time.UnixMilli(o.GetUpdatedUnixMs())}
}

func slots32(items []slotIn) []int32 {
	out := make([]int32, 0, len(items))
	for _, it := range items {
		out = append(out, int32(it.Slot))
	}
	return out
}

func names(items []slotIn) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, strconv.Itoa(it.Slot))
	}
	return out
}

func keys(os []owner) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.key)
	}
	return out
}

// friendOwner names a friend (or the caller) found by a friend query; false when the account has
// no PS3 link, so it has no JID to show.
func (s *Service) friendOwner(c *call, account string) (owner, bool) {
	if account == c.actor.AccountID {
		return owner{key: account, jid: c.actorJID, account: account, online: c.actor.OnlineID}, true
	}
	link, err := s.Core.GetLinkByAccount(c.r.Context(), account)
	if err != nil {
		return owner{}, false
	}
	return owner{key: account, jid: s.jid(c.r.Context(), account, link.OnlineID), account: account, online: link.OnlineID}, true
}

// page is the friend-query paging: sort date|variable (data: date|size), order asc|desc.
func page(c *call, bySize bool) (sort string, asc bool, offset, limit uint32) {
	sort = "updated"
	if c.req.Option.Sort == "variable" {
		sort = "value"
		if bySize {
			sort = "size"
		}
	}
	l := c.req.Option.Limit
	if l <= 0 || l > 100 {
		l = 100
	}
	return sort, c.req.Option.Order == "asc", uint32(max(c.req.Option.Offset, 0)), uint32(l)
}

func (s *Service) setMultiSlotVariable(c *call) {
	o, ok := s.single(c)
	if !ok {
		return
	}
	var items []*pb.VariableItem
	for _, it := range c.req.List.Variables {
		v, err := strconv.ParseInt(it.Value, 10, 64)
		if err != nil {
			reply(c.w, resultUnspecified, nil)
			return
		}
		items = append(items, &pb.VariableItem{Slot: int32(it.Slot), Value: v})
	}
	if len(items) == 0 || len(items) > maxSlots {
		reply(c.w, resultUnspecified, nil)
		return
	}
	if _, err := s.Storage.SetVariables(c.r.Context(), &pb.SetVariablesRequest{Ref: ref(c.req.CommID), Owner: o.key,
		Writer: c.actor.AccountID, Setter: c.actorJID, Items: items, Visibility: vis}); err != nil {
		s.fail(c, err)
		return
	}
	reply(c.w, resultOK, nil)
}

// variables lists the given owners' slots, in the order of owners then slots.
func (s *Service) variables(c *call, owners []owner, slots []int32) ([]any, bool) {
	res, err := s.Storage.ListVariables(c.r.Context(), &pb.ListVariablesRequest{Ref: ref(c.req.CommID), Viewer: c.actor.AccountID,
		Owners: keys(owners), Slots: slots, Limit: 1000})
	if err != nil {
		s.fail(c, err)
		return nil, false
	}
	type id struct {
		owner string
		slot  int32
	}
	got := map[id]*pb.Variable{}
	for _, v := range res.Variables {
		got[id{v.Owner, v.Slot}] = v
	}
	var items []any
	for _, o := range owners {
		for _, slot := range slots {
			if v, ok := got[id{o.key, slot}]; ok {
				items = append(items, variableOut(o, variableFrom(v), nil))
			}
		}
	}
	return items, true
}

func (s *Service) getMultiSlotVariable(c *call) {
	o, ok := s.single(c)
	if !ok {
		return
	}
	items, ok := s.variables(c, []owner{o}, slots32(c.req.List.Variables))
	if ok {
		reply(c.w, resultOK, listOut(-1, items))
	}
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
	items, ok := s.variables(c, targets, []int32{int32(c.req.Variable.Slot)})
	if ok {
		reply(c.w, resultOK, listOut(-1, items))
	}
}

func (s *Service) getFriendUserVariable(c *call) {
	if c.req.Variable == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	sort, asc, offset, limit := page(c, false)
	res, err := s.Storage.ListVariables(c.r.Context(), &pb.ListVariablesRequest{Ref: ref(c.req.CommID), Viewer: c.actor.AccountID,
		FriendsOfViewer: true, IncludeSelf: c.req.Option.Owner == "true", Slots: []int32{int32(c.req.Variable.Slot)},
		Sort: sort, Ascending: asc, Offset: offset, Limit: limit})
	if err != nil {
		s.fail(c, err)
		return
	}
	var items []any
	for _, v := range res.Variables {
		if o, ok := s.friendOwner(c, v.Owner); ok { // a friend without an NP link has no PS3 storage
			items = append(items, variableOut(o, variableFrom(v), nil))
		}
	}
	reply(c.w, resultOK, listOut(int(res.Hits), items))
}

// varResult renders a TrySet/Add result; a slot that does not exist is a zero row.
func (s *Service) varResult(c *call, o owner, slot int, r *pb.VariableResult, withOld bool) {
	if r.ConditionFailed {
		reply(c.w, resultCondition, nil)
		return
	}
	v := Variable{Slot: slot, Setter: c.actorJID, Updated: time.Now()}
	if r.Variable != nil {
		v = variableFrom(r.Variable)
	}
	var old *int64
	if withOld {
		old = &r.PreviousValue
	}
	reply(c.w, resultOK, variableOut(o, v, old))
}

// trysetVariable stores value when "operand <try> current" holds (missing counts as 0); comp-value,
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
	o, ok := s.single(c)
	if !ok {
		return
	}
	r, err := s.Storage.TrySetVariable(c.r.Context(), &pb.TrySetVariableRequest{Ref: ref(c.req.CommID), Owner: o.key, Writer: c.actor.AccountID,
		Setter: c.actorJID, Slot: int32(it.Slot), Value: value, Op: it.Try, Operand: operand, Condition: c.condition(), Visibility: vis})
	if err != nil {
		s.fail(c, err)
		return
	}
	s.varResult(c, o, it.Slot, r, true)
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
	o, ok := s.single(c)
	if !ok {
		return
	}
	r, err := s.Storage.AddVariable(c.r.Context(), &pb.AddVariableRequest{Ref: ref(c.req.CommID), Owner: o.key, Writer: c.actor.AccountID,
		Setter: c.actorJID, Slot: int32(it.Slot), Delta: delta, Condition: c.condition(), Visibility: vis})
	if err != nil {
		s.fail(c, err)
		return
	}
	s.varResult(c, o, it.Slot, r, false)
}

func (s *Service) deleteMultiSlotVariable(c *call) {
	o, ok := s.single(c)
	if !ok {
		return
	}
	if len(c.req.List.Variables) == 0 {
		reply(c.w, resultOK, nil)
		return
	}
	if _, err := s.Storage.DeleteVariables(c.r.Context(), &pb.DeleteVariablesRequest{Ref: ref(c.req.CommID), Owner: o.key,
		Writer: c.actor.AccountID, Slots: slots32(c.req.List.Variables)}); err != nil {
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
	o, ok := s.single(c)
	if !ok {
		return
	}
	r, err := s.Storage.PutObject(c.r.Context(), &pb.PutObjectRequest{Ref: ref(c.req.CommID), Owner: o.key, Writer: c.actor.AccountID,
		Setter: c.actorJID, Collection: dataColl, Name: strconv.Itoa(it.Slot), Info: info, Body: c.data, Visibility: vis, Condition: c.condition()})
	if err != nil {
		s.fail(c, err)
		return
	}
	if r.ConditionFailed {
		reply(c.w, resultCondition, nil)
		return
	}
	reply(c.w, resultOK, nil)
}

func (s *Service) getData(c *call) {
	if c.req.Data == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	o, ok := s.single(c)
	if !ok {
		return
	}
	r, err := s.Storage.GetObject(c.r.Context(), &pb.GetObjectRequest{Ref: ref(c.req.CommID), Viewer: c.actor.AccountID, Owner: o.key,
		Collection: dataColl, Name: strconv.Itoa(c.req.Data.Slot), IncludeBody: true})
	if err != nil {
		s.fail(c, err)
		return
	}
	if !r.Found {
		reply(c.w, resultNoData, nil)
		return
	}
	x := envelope(resultOK, dataOut(o, dataFrom(r.Object), false))
	c.w.Header().Set("Sce-Np-Info-Length", strconv.Itoa(len(x)))
	body := append(append(x, "\r\n\r\n"...), r.Object.Body...)
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.Write(body)
}

// dataStatus lists slot metadata (no bodies) of the given owners, in owner then slot order.
func (s *Service) dataStatus(c *call, owners []owner, names []string) ([]any, bool) {
	res, err := s.Storage.ListObjects(c.r.Context(), &pb.ListObjectsRequest{Ref: ref(c.req.CommID), Viewer: c.actor.AccountID,
		Owners: keys(owners), Collection: dataColl, Names: names, Limit: 1000})
	if err != nil {
		s.fail(c, err)
		return nil, false
	}
	got := map[string]*pb.Object{}
	for _, o := range res.Objects {
		got[o.Owner+"|"+o.Name] = o
	}
	var items []any
	for _, o := range owners {
		for _, n := range names {
			if x, ok := got[o.key+"|"+n]; ok {
				items = append(items, dataOut(o, dataFrom(x), true))
			}
		}
	}
	return items, true
}

func (s *Service) getMultiSlotDataStatus(c *call) {
	o, ok := s.single(c)
	if !ok {
		return
	}
	items, ok := s.dataStatus(c, []owner{o}, names(c.req.List.Data))
	if ok {
		reply(c.w, resultOK, listOut(-1, items))
	}
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
	items, ok := s.dataStatus(c, targets, []string{strconv.Itoa(c.req.Data.Slot)})
	if ok {
		reply(c.w, resultOK, listOut(-1, items))
	}
}

func (s *Service) getFriendUserDataStatus(c *call) {
	if c.req.Data == nil {
		reply(c.w, resultUnspecified, nil)
		return
	}
	sort, asc, offset, limit := page(c, true)
	res, err := s.Storage.ListObjects(c.r.Context(), &pb.ListObjectsRequest{Ref: ref(c.req.CommID), Viewer: c.actor.AccountID,
		FriendsOfViewer: true, IncludeSelf: c.req.Option.Owner == "true", Collection: dataColl, Names: []string{strconv.Itoa(c.req.Data.Slot)},
		Sort: sort, Ascending: asc, Offset: offset, Limit: limit})
	if err != nil {
		s.fail(c, err)
		return
	}
	var items []any
	for _, x := range res.Objects {
		if o, ok := s.friendOwner(c, x.Owner); ok {
			items = append(items, dataOut(o, dataFrom(x), true))
		}
	}
	reply(c.w, resultOK, listOut(int(res.Hits), items))
}

func (s *Service) deleteMultiSlotData(c *call) {
	o, ok := s.single(c)
	if !ok {
		return
	}
	if len(c.req.List.Data) == 0 { // an empty name list would delete the whole collection
		reply(c.w, resultOK, nil)
		return
	}
	if _, err := s.Storage.DeleteObjects(c.r.Context(), &pb.DeleteObjectsRequest{Ref: ref(c.req.CommID), Owner: o.key, Writer: c.actor.AccountID,
		Collection: dataColl, Names: names(c.req.List.Data)}); err != nil {
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
