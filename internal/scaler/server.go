package scaler

import (
	"context"
	"net"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Server is a controller-runtime manager.Runnable that serves WVA's KEDA
// external scaler over gRPC.
//
// It is registered leader-gated (co-located with the optimize loop that feeds
// the decision store), so on a single-replica deployment the active instance
// serves KEDA. Multi-replica HA serving (routing KEDA to the leader, or a
// shared decision store) is out of scope for this MVP.
type Server struct {
	// Addr is the gRPC bind address, e.g. ":9090".
	Addr string
	// Client reads KEDA ScaledObjects to resolve scale targets.
	Client client.Client
}

// Start listens and serves until ctx is cancelled, then stops gracefully.
// It implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("external-scaler")

	lis, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	pb.RegisterExternalScalerServer(grpcServer, NewHandler(s.Client, nil))
	logger.Info("KEDA external scaler listening", "addr", s.Addr)

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()

	select {
	case <-ctx.Done():
		grpcServer.GracefulStop()
		logger.Info("KEDA external scaler stopped")
		return nil
	case err := <-serveErr:
		return err
	}
}
