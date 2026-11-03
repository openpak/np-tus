// Package fakestorage is an in-process openpak.titlestorage.v1.TitleStorage gRPC server for
// tests: variables and objects live in memory, and the rules np-tus depends on follow the real
// service (visibility, the write rule, try-set operators, write conditions, friend scopes with
// sort and paging, the 10 ms write stamp, the x-internal-key check).
package fakestorage

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	pb "openpak/np-tus/internal/titlestoragepb"
)

type vkey struct {
	title, owner string
	slot         int32
}

type okey struct{ title, owner, coll, name string }

type Fake struct {
	pb.UnimplementedTitleStorageServer
	Key string
	// Down makes every call fail Unavailable.
	Down    bool
	mu      sync.Mutex
	vars    map[vkey]*pb.Variable
	objs    map[okey]*pb.Object
	friends map[string][]string
	blocked map[string]bool
	nextID  uint64
}

func New(apiKey string) *Fake {
	return &Fake{Key: apiKey, vars: map[vkey]*pb.Variable{}, objs: map[okey]*pb.Object{}, friends: map[string][]string{}, blocked: map[string]bool{}}
}

func (f *Fake) AddFriend(a, b string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.friends[a] = append(f.friends[a], b)
	f.friends[b] = append(f.friends[b], a)
}

// SetBlocked makes writes by actor to target fail PermissionDenied.
func (f *Fake) SetBlocked(actor, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[actor+"|"+target] = true
}

// Object returns a stored object (nil when none).
func (f *Fake) Object(title, owner, coll, name string) *pb.Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[okey{title, owner, coll, name}]
}

// Serve starts the fake and returns dial options for a client.
func (f *Fake) Serve(t testing.TB) []grpc.DialOption {
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterTitleStorageServer(gs, f)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() })}
}

func (f *Fake) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("x-internal-key"); len(v) == 0 || v[0] != f.Key {
		return status.Error(codes.Unauthenticated, "missing internal credentials")
	}
	if f.Down {
		return status.Error(codes.Unavailable, "down")
	}
	return nil
}

func now() int64 { return time.Now().UnixMilli() / 10 * 10 }

func virtual(o string) bool { return strings.HasPrefix(o, "v:") }

func (f *Fake) canWrite(writer, owner string) error {
	if virtual(owner) || writer == owner || !f.blocked[writer+"|"+owner] {
		return nil
	}
	return status.Error(codes.PermissionDenied, "blocked")
}

func (f *Fake) canRead(viewer, owner string, vis pb.Visibility) bool {
	if vis == pb.Visibility_VISIBILITY_PUBLIC || virtual(owner) || viewer == owner {
		return true
	}
	if vis != pb.Visibility_VISIBILITY_FRIENDS || viewer == "" {
		return false
	}
	for _, fr := range f.friends[viewer] {
		if fr == owner {
			return true
		}
	}
	return false
}

func holds(c *pb.Condition, exists bool, updated int64, setter string) bool {
	if c == nil {
		return true
	}
	if c.ExpectUpdatedUnixMs != nil && (!exists || updated != c.GetExpectUpdatedUnixMs()) {
		return false
	}
	return c.ExpectSetter == nil || (exists && setter == c.GetExpectSetter())
}

func (f *Fake) SetVariables(ctx context.Context, r *pb.SetVariablesRequest) (*pb.SetVariablesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if err := f.canWrite(r.Writer, r.Owner); err != nil {
		return nil, err
	}
	for _, it := range r.Items {
		f.vars[vkey{r.Ref.Title, r.Owner, it.Slot}] = &pb.Variable{Owner: r.Owner, Slot: it.Slot, Value: it.Value, Setter: r.Setter, UpdatedUnixMs: now(), Visibility: r.Visibility}
	}
	return &pb.SetVariablesResponse{}, nil
}

func cmp(op string, a, b int64) bool {
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

// write is the shared body of AddVariable / TrySetVariable.
func (f *Fake) write(ctx context.Context, ref *pb.TitleRef, owner, writer, setter string, slot int32, vis pb.Visibility, c *pb.Condition, fn func(cur int64) (int64, bool)) (*pb.VariableResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if err := f.canWrite(writer, owner); err != nil {
		return nil, err
	}
	k := vkey{ref.Title, owner, slot}
	cur := f.vars[k]
	res := &pb.VariableResult{}
	var cv int64
	if cur != nil {
		cv, res.HadPrevious, res.PreviousValue = cur.Value, true, cur.Value
	}
	if !holds(c, cur != nil, cur.GetUpdatedUnixMs(), cur.GetSetter()) {
		res.ConditionFailed, res.Variable = true, cur
		return res, nil
	}
	if v, ok := fn(cv); ok {
		f.vars[k] = &pb.Variable{Owner: owner, Slot: slot, Value: v, Setter: setter, UpdatedUnixMs: now(), Visibility: vis}
		res.Stored = true
	}
	res.Variable = f.vars[k]
	return res, nil
}

func (f *Fake) AddVariable(ctx context.Context, r *pb.AddVariableRequest) (*pb.VariableResult, error) {
	return f.write(ctx, r.Ref, r.Owner, r.Writer, r.Setter, r.Slot, r.Visibility, r.Condition, func(cur int64) (int64, bool) { return cur + r.Delta, true })
}

func (f *Fake) TrySetVariable(ctx context.Context, r *pb.TrySetVariableRequest) (*pb.VariableResult, error) {
	return f.write(ctx, r.Ref, r.Owner, r.Writer, r.Setter, r.Slot, r.Visibility, r.Condition, func(cur int64) (int64, bool) { return r.Value, cmp(r.Op, r.Operand, cur) })
}

func (f *Fake) DeleteVariables(ctx context.Context, r *pb.DeleteVariablesRequest) (*pb.DeleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if err := f.canWrite(r.Writer, r.Owner); err != nil {
		return nil, err
	}
	n := uint32(0)
	for _, s := range r.Slots {
		if _, ok := f.vars[vkey{r.Ref.Title, r.Owner, s}]; ok {
			delete(f.vars, vkey{r.Ref.Title, r.Owner, s})
			n++
		}
	}
	return &pb.DeleteResponse{Deleted: n}, nil
}

// scope is the owner list of a read: the given owners, or the viewer's friends (plus the viewer).
func (f *Fake) scope(viewer string, owners []string, friends, self bool) []string {
	if !friends {
		return owners
	}
	out := append([]string(nil), f.friends[viewer]...)
	if self {
		out = append(out, viewer)
	}
	return out
}

func page[T any](xs []T, offset, limit uint32) []T {
	if limit == 0 {
		limit = 100
	}
	if int(offset) >= len(xs) {
		return nil
	}
	xs = xs[offset:]
	if len(xs) > int(limit) {
		xs = xs[:limit]
	}
	return xs
}

func (f *Fake) ListVariables(ctx context.Context, r *pb.ListVariablesRequest) (*pb.ListVariablesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	var got []*pb.Variable
	for _, o := range f.scope(r.Viewer, r.Owners, r.FriendsOfViewer, r.IncludeSelf) {
		for k, v := range f.vars {
			if k.title != r.Ref.Title || k.owner != o || !f.canRead(r.Viewer, o, v.Visibility) {
				continue
			}
			if len(r.Slots) > 0 && !contains(r.Slots, k.slot) {
				continue
			}
			got = append(got, v)
		}
	}
	sort.SliceStable(got, func(i, j int) bool {
		a, b := got[i], got[j]
		if r.Ascending {
			a, b = b, a
		}
		switch r.Sort {
		case "value":
			return a.Value > b.Value
		case "slot":
			return a.Slot > b.Slot
		}
		return a.UpdatedUnixMs > b.UpdatedUnixMs
	})
	return &pb.ListVariablesResponse{Variables: page(got, r.Offset, r.Limit), Hits: uint32(len(got))}, nil
}

func contains(xs []int32, x int32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (f *Fake) PutObject(ctx context.Context, r *pb.PutObjectRequest) (*pb.ObjectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if err := f.canWrite(r.Writer, r.Owner); err != nil {
		return nil, err
	}
	if len(r.Body) > 4<<20 {
		return nil, status.Error(codes.ResourceExhausted, "value too large")
	}
	name := r.Name
	k := okey{r.Ref.Title, r.Owner, r.Collection, name}
	cur := f.objs[k]
	if !holds(r.Condition, cur != nil, cur.GetUpdatedUnixMs(), cur.GetSetter()) {
		return &pb.ObjectResponse{ConditionFailed: true}, nil
	}
	id := cur.GetId()
	if cur == nil {
		f.nextID++
		id = f.nextID
		if name == "" {
			name = strconv.FormatUint(id, 10)
			k.name = name
		}
	}
	o := &pb.Object{Id: id, Title: r.Ref.Title, Owner: r.Owner, Collection: r.Collection, Name: name, Info: r.Info, Body: r.Body,
		Size: uint64(len(r.Body)), Setter: r.Setter, Visibility: r.Visibility, UpdatedUnixMs: now()}
	f.objs[k] = o
	return &pb.ObjectResponse{Object: o, Found: true}, nil
}

func (f *Fake) GetObject(ctx context.Context, r *pb.GetObjectRequest) (*pb.ObjectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	o := f.objs[okey{r.Ref.Title, r.Owner, r.Collection, r.Name}]
	if o == nil || !f.canRead(r.Viewer, o.Owner, o.Visibility) {
		return &pb.ObjectResponse{}, nil
	}
	return &pb.ObjectResponse{Object: o, Found: true}, nil
}

func (f *Fake) ListObjects(ctx context.Context, r *pb.ListObjectsRequest) (*pb.ListObjectsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	var got []*pb.Object
	for _, ow := range f.scope(r.Viewer, r.Owners, r.FriendsOfViewer, r.IncludeSelf) {
		for k, o := range f.objs {
			if k.title != r.Ref.Title || k.owner != ow || !f.canRead(r.Viewer, ow, o.Visibility) {
				continue
			}
			if r.Collection != "" && k.coll != r.Collection {
				continue
			}
			if len(r.Names) > 0 && !containsS(r.Names, k.name) {
				continue
			}
			c := proto.Clone(o).(*pb.Object)
			if !r.IncludeBody {
				c.Body = nil
			}
			got = append(got, c)
		}
	}
	sort.SliceStable(got, func(i, j int) bool {
		a, b := got[i], got[j]
		if r.Ascending {
			a, b = b, a
		}
		if r.Sort == "size" {
			return a.Size > b.Size
		}
		return a.UpdatedUnixMs > b.UpdatedUnixMs
	})
	return &pb.ListObjectsResponse{Objects: page(got, r.Offset, r.Limit), Hits: uint32(len(got))}, nil
}

func containsS(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (f *Fake) DeleteObjects(ctx context.Context, r *pb.DeleteObjectsRequest) (*pb.DeleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if err := f.canWrite(r.Writer, r.Owner); err != nil {
		return nil, err
	}
	n := uint32(0)
	for k := range f.objs {
		if k.title == r.Ref.Title && k.owner == r.Owner && k.coll == r.Collection && (len(r.Names) == 0 || containsS(r.Names, k.name)) {
			delete(f.objs, k)
			n++
		}
	}
	return &pb.DeleteResponse{Deleted: n}, nil
}
