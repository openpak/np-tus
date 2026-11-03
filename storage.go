package main

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "openpak/np-tus/internal/titlestoragepb"
)

// DialStorage connects to the neutral title-storage service (gRPC) and presents the internal
// key on every call. extra lets tests add a dialer.
func DialStorage(addr, key string, extra ...grpc.DialOption) (pb.TitleStorageClient, *grpc.ClientConn, error) {
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, o ...grpc.CallOption) error {
			return inv(metadata.AppendToOutgoingContext(ctx, "x-internal-key", key), method, req, reply, cc, o...)
		}),
	}, extra...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, nil, err
	}
	return pb.NewTitleStorageClient(conn), conn, nil
}
