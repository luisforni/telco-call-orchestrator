package grpc

import (
	"context"
	"fmt"
	"net"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

type Server struct {
	srv    *grpc.Server
	health *health.Server
	port   int
	log    *zap.Logger
}

func NewServer(port int, log *zap.Logger) *Server {
	s := grpc.NewServer(
		grpc.UnaryInterceptor(loggingInterceptor(log)),
	)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s, hs)
	reflection.Register(s)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return &Server{srv: s, health: hs, port: port, log: log}
}

func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	s.log.Info("gRPC server listening", zap.Int("port", s.port))
	go func() {
		<-ctx.Done()
		s.srv.GracefulStop()
	}()
	return s.srv.Serve(lis)
}

func loggingInterceptor(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			log.Error("gRPC error", zap.String("method", info.FullMethod), zap.Error(err))
		}
		return resp, err
	}
}
