package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apiv1 "viper/api/gen/v1"
	"viper/internal/observability"
	"viper/internal/security"
	"viper/pkg/contracts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const defaultPublicMaxConcurrent = 256
const defaultPublicRequestsPerSecond = 1000

// GRPCServerConfig configures the authenticated client-facing service. TLS
// and an explicit authorization policy are mandatory for every public server.
type GRPCServerConfig struct {
	Credentials          credentials.TransportCredentials
	Policy               *security.Policy
	MaxConcurrent        int
	MaxRequestsPerSecond int
	Metrics              *observability.CounterSet
	Options              []grpc.ServerOption
}

// NewGRPCServer creates a bounded, authenticated gRPC server and registers
// the supplied public services. At least one service must be provided.
func NewGRPCServer(config GRPCServerConfig, kv apiv1.KVServiceServer, statusService apiv1.NodeStatusServiceServer) (*grpc.Server, error) {
	if config.Credentials == nil || config.Credentials.Info().SecurityProtocol != "tls" {
		return nil, fmt.Errorf("%w: TLS credentials are required", ErrInvalidService)
	}
	if config.Policy == nil {
		return nil, fmt.Errorf("%w: authorization policy is required", ErrInvalidService)
	}
	if kv == nil && statusService == nil {
		return nil, fmt.Errorf("%w: no public service is registered", ErrInvalidService)
	}
	maxConcurrent := config.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = defaultPublicMaxConcurrent
	}
	if maxConcurrent < 1 || maxConcurrent > 4096 {
		return nil, fmt.Errorf("%w: max concurrent requests must be between 1 and 4096", ErrInvalidService)
	}
	maxRequestsPerSecond := config.MaxRequestsPerSecond
	if maxRequestsPerSecond == 0 {
		maxRequestsPerSecond = defaultPublicRequestsPerSecond
	}
	if maxRequestsPerSecond < 1 || maxRequestsPerSecond > 1_000_000 {
		return nil, fmt.Errorf("%w: max requests per second must be between 1 and 1000000", ErrInvalidService)
	}
	rateLimiter := newRequestRateLimiter(maxRequestsPerSecond)
	options := append([]grpc.ServerOption(nil), config.Options...)
	options = append(options,
		grpc.Creds(config.Credentials),
		grpc.MaxRecvMsgSize(contracts.MaxRPCMessageBytes),
		grpc.MaxSendMsgSize(contracts.MaxRPCMessageBytes),
		grpc.ChainUnaryInterceptor(
			boundedUnaryInterceptor(maxConcurrent, rateLimiter, config.Metrics),
			authorizedUnaryInterceptor(config.Policy),
		),
	)
	server := grpc.NewServer(options...)
	if kv != nil {
		apiv1.RegisterKVServiceServer(server, kv)
	}
	if statusService != nil {
		apiv1.RegisterNodeStatusServiceServer(server, statusService)
	}
	return server, nil
}

// NewKVGRPCServer is a convenience wrapper for a KV-only public endpoint.
func NewKVGRPCServer(config GRPCServerConfig, kv apiv1.KVServiceServer) (*grpc.Server, error) {
	return NewGRPCServer(config, kv, nil)
}

func boundedUnaryInterceptor(maxConcurrent int, rateLimiter *requestRateLimiter, metrics *observability.CounterSet) grpc.UnaryServerInterceptor {
	limit := make(chan struct{}, maxConcurrent)
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if metrics != nil {
			metrics.Add("requests_total", 1)
		}
		if !rateLimiter.allow() {
			if metrics != nil {
				metrics.Add("request_rate_limited_total", 1)
			}
			return nil, status.Error(codes.ResourceExhausted, "service rate limit exceeded")
		}
		select {
		case limit <- struct{}{}:
			defer func() { <-limit }()
		default:
			if metrics != nil {
				metrics.Add("request_errors_total", 1)
			}
			return nil, status.Error(codes.ResourceExhausted, "service resource limit exceeded")
		}
		response, err := handler(ctx, request)
		if err != nil && metrics != nil {
			metrics.Add("request_errors_total", 1)
		}
		return response, err
	}
}

type requestRateLimiter struct {
	mu     sync.Mutex
	limit  float64
	tokens float64
	last   time.Time
}

func newRequestRateLimiter(limit int) *requestRateLimiter {
	return &requestRateLimiter{limit: float64(limit), tokens: float64(limit), last: time.Now()}
}

func (r *requestRateLimiter) allow() bool {
	if r == nil {
		return false
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if elapsed := now.Sub(r.last).Seconds(); elapsed > 0 {
		r.tokens += elapsed * r.limit
		if r.tokens > r.limit {
			r.tokens = r.limit
		}
		r.last = now
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}

func authorizedUnaryInterceptor(policy *security.Policy) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		principal, err := principalFromContext(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "authentication required")
		}
		operation, ok := operationForMethod(info.FullMethod)
		if !ok || policy.Authorize(principal, operation) != nil {
			return nil, status.Error(codes.PermissionDenied, "permission denied")
		}
		return handler(ctx, request)
	}
}

func operationForMethod(method string) (security.Operation, bool) {
	switch method {
	case "/viper.api.v1.KVService/Get":
		return security.OperationRead, true
	case "/viper.api.v1.KVService/Put", "/viper.api.v1.KVService/CompareAndSwap":
		return security.OperationMutate, true
	case "/viper.api.v1.NodeStatusService/GetNodeStatus":
		return security.OperationAdmin, true
	default:
		return "", false
	}
}

func principalFromContext(ctx context.Context) (security.Principal, error) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo == nil {
		return security.Principal{}, errors.New("missing peer identity")
	}
	var tlsInfo credentials.TLSInfo
	switch info := peerInfo.AuthInfo.(type) {
	case credentials.TLSInfo:
		tlsInfo = info
	case *credentials.TLSInfo:
		if info == nil {
			return security.Principal{}, errors.New("missing TLS identity")
		}
		tlsInfo = *info
	default:
		return security.Principal{}, errors.New("non-TLS peer")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return security.Principal{}, errors.New("missing client certificate")
	}
	return security.PrincipalFromCertificate(tlsInfo.State.PeerCertificates[0])
}
