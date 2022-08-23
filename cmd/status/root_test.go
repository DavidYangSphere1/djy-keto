package status

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ory/x/cmdx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	grpcHealthV1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	"github.com/ory/keto/cmd/client"
	"github.com/ory/keto/internal/driver"
	"github.com/ory/keto/internal/namespace"
)

func TestStatusCmd(t *testing.T) {
	for _, serverType := range []client.ServerType{client.ReadServer, client.WriteServer} {
		t.Run("server_type="+string(serverType), func(t *testing.T) {
			ts := client.NewTestServer(t, serverType, []*namespace.Namespace{{Name: t.Name()}}, newStatusCmd)
			defer ts.Shutdown(t)
			ts.Cmd.PersistentArgs = append(ts.Cmd.PersistentArgs, "--"+cmdx.FlagQuiet, "--"+FlagEndpoint, string(serverType))

			t.Run("case=timeout,noblock", func(t *testing.T) {
				t.Skip()
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()

				stdOut := cmdx.ExecNoErrCtx(ctx, t, newStatusCmd(), "--"+FlagEndpoint, string(serverType), "--"+ts.FlagRemote, ts.Addr+"0")
				assert.Equal(t, grpcHealthV1.HealthCheckResponse_NOT_SERVING.String()+"\n", stdOut)
			})

			t.Run("case=noblock", func(t *testing.T) {
				t.Skip()
				stdOut := ts.Cmd.ExecNoErr(t)
				assert.Equal(t, grpcHealthV1.HealthCheckResponse_SERVING.String()+"\n", stdOut)
			})

			t.Run("case=block", func(t *testing.T) {
				ctx := context.WithValue(context.Background(), client.ContextKeyTimeout, time.Second)

				l, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				s := ts.NewServer(ctx)

				startServe := make(chan struct{})

				serveErr := &errgroup.Group{}
				serveErr.Go(func() error {
					// wait until we get the signal to start
					<-startServe
					return s.Serve(l)
				})

				var stdIn, stdErr bytes.Buffer
				stdOut := cmdx.CallbackWriter{
					Callbacks: map[string]func([]byte) error{
						// once we get the first retry message, we want to start serving
						"Context deadline exceeded, going to retry.": func([]byte) error {
							// select ensures we only call this if the chan is not already closed
							select {
							case <-startServe:
							default:
								close(startServe)
							}
							return nil
						},
					},
				}

				require.NoError(t,
					cmdx.ExecBackgroundCtx(ctx, newStatusCmd(), &stdIn, &stdOut, &stdErr,
						"--"+FlagEndpoint, string(serverType),
						"--"+ts.FlagRemote, l.Addr().String(),
						"--insecure-skip-hostname-verification=true",
						"--"+FlagBlock,
					).Wait(),
				)

				fullStdOut := stdOut.String()
				assert.True(t, strings.HasSuffix(fullStdOut, "\n"+grpcHealthV1.HealthCheckResponse_SERVING.String()+"\n"), fullStdOut)

				s.GracefulStop()
				require.NoError(t, serveErr.Wait())
			})
		})
	}
}

func authInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, errors.New("not authorized, no metadata")
	}
	vals := md.Get("authorization")
	if len(vals) != 1 {
		return nil, errors.New("not authorized, no authorization header")
	}
	if vals[0] != "Bearer secret" {
		return nil, errors.New("not authorized")
	}
	return handler(ctx, req)
}

func TestAuthorizedRequest(t *testing.T) {
	ts := client.NewTestServer(
		t, "read", []*namespace.Namespace{{Name: t.Name()}}, newStatusCmd,
		driver.WithGRPCUnaryInterceptors(authInterceptor),
	)
	defer ts.Shutdown(t)

	t.Run("case=not authorized", func(t *testing.T) {
		out := ts.Cmd.ExecExpectedErr(t)
		assert.Contains(t, out, "not authorized")
	})

	t.Run("case=authorized", func(t *testing.T) {
		t.Setenv("ORY_PAT", "secret")
		out := ts.Cmd.ExecNoErr(t)
		assert.Contains(t, out, "SERVING")
	})
}
