package grpcmock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"reflect"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	grpcReflect "github.com/nhatthm/grpcmock/reflect"
)

// ContextDialer is to set up the dialer.
type ContextDialer = func(context.Context, string) (net.Conn, error)

// ClientStreamHandler handles a client stream.
type ClientStreamHandler func(stream grpc.ClientStream) error

// Handle handles a client stream.
func (h ClientStreamHandler) Handle(stream grpc.ClientStream) error {
	if h == nil {
		return nil
	}

	return h(stream)
}

type invokeConfig struct {
	header   map[string]string
	dialOpts []grpc.DialOption
	callOpts []grpc.CallOption
}

// InvokeOption sets invoker config.
type InvokeOption func(c *invokeConfig)

// InvokeUnary invokes a unary method.
func InvokeUnary(
	ctx context.Context,
	method string,
	in interface{},
	out interface{},
	opts ...InvokeOption,
) error {
	ctx, conn, method, callOpts, err := prepInvoke(ctx, method, opts...)
	if err != nil {
		return err
	}

	return conn.Invoke(ctx, method, in, out, callOpts...)
}

// InvokeServerStream invokes a server-stream method.
func InvokeServerStream(
	ctx context.Context,
	method string,
	in interface{},
	handle ClientStreamHandler,
	opts ...InvokeOption,
) error {
	ctx, conn, method, callOpts, err := prepInvoke(ctx, method, opts...)
	if err != nil {
		return err
	}

	desc := &grpc.StreamDesc{ServerStreams: true}

	stream, err := conn.NewStream(ctx, desc, method, callOpts...)
	if err != nil {
		return err
	}

	if err := stream.SendMsg(in); err != nil {
		return err
	}

	if err := stream.CloseSend(); err != nil {
		return err
	}

	return handle.Handle(stream)
}

// InvokeClientStream invokes a client-stream method.
func InvokeClientStream(
	ctx context.Context,
	method string,
	handle ClientStreamHandler,
	out interface{},
	opts ...InvokeOption,
) error {
	ctx, conn, method, callOpts, err := prepInvoke(ctx, method, opts...)
	if err != nil {
		return err
	}

	desc := &grpc.StreamDesc{ClientStreams: true}

	stream, err := conn.NewStream(ctx, desc, method, callOpts...)
	if err != nil {
		return err
	}

	if err := handle.Handle(stream); err != nil {
		return err
	}

	if err := stream.CloseSend(); err != nil {
		return err
	}

	return stream.RecvMsg(out)
}

func prepInvoke(ctx context.Context, method string, opts ...InvokeOption) (context.Context, *grpc.ClientConn, string, []grpc.CallOption, error) {
	addr, method, err := parseMethod(method)
	if err != nil {
		return ctx, nil, "", nil, fmt.Errorf("coulld not parse method url: %w", err)
	}

	ctx, dialOpts, callOpts := invokeOptions(ctx, opts...)

	conn, err := grpc.DialContext(ctx, addr, dialOpts...)
	if err != nil {
		return ctx, nil, "", nil, err
	}

	return ctx, conn, method, callOpts, err
}

func parseMethod(method string) (string, string, error) {
	u, err := url.Parse(method)
	if err != nil {
		return "", "", err
	}

	method = fmt.Sprintf("/%s", strings.TrimLeft(u.Path, "/"))

	if method == "/" {
		return "", "", ErrMissingMethod
	}

	addr := url.URL{
		Scheme: u.Scheme,
		User:   u.User,
		Host:   u.Host,
	}

	return addr.String(), method, nil
}

func invokeOptions(ctx context.Context, opts ...InvokeOption) (context.Context, []grpc.DialOption, []grpc.CallOption) {
	cfg := invokeConfig{
		header: map[string]string{},
	}

	for _, o := range opts {
		o(&cfg)
	}

	if len(cfg.header) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(cfg.header))
	}

	return ctx, cfg.dialOpts, cfg.callOpts
}

// WithHeader sets request header.
func WithHeader(key, value string) InvokeOption {
	return func(c *invokeConfig) {
		c.header[key] = value
	}
}

// WithHeaders sets request header.
func WithHeaders(header map[string]string) InvokeOption {
	return func(c *invokeConfig) {
		for k, v := range header {
			c.header[k] = v
		}
	}
}

// WithContextDialer sets a context dialer to create connections.
//
// See:
// 	- grpcmock.WithBufConnDialer()
func WithContextDialer(d ContextDialer) InvokeOption {
	return WithDialOptions(grpc.WithContextDialer(d))
}

// WithBufConnDialer sets a *bufconn.Listener as the context dialer.
//
// See:
// 	- grpcmock.WithContextDialer()
func WithBufConnDialer(l *bufconn.Listener) InvokeOption {
	return WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return l.Dial()
	})
}

// WithInsecure disables transport security for the connections.
func WithInsecure() InvokeOption {
	return WithDialOptions(grpc.WithInsecure())
}

// WithDialOptions sets dial options.
func WithDialOptions(opts ...grpc.DialOption) InvokeOption {
	return func(c *invokeConfig) {
		c.dialOpts = append(c.dialOpts, opts...)
	}
}

// WithCallOption sets call options.
func WithCallOption(opts ...grpc.CallOption) InvokeOption {
	return func(c *invokeConfig) {
		c.callOpts = append(c.callOpts, opts...)
	}
}

// SendAll sends everything to the stream.
func SendAll(in interface{}) ClientStreamHandler {
	return func(stream grpc.ClientStream) error {
		if err := grpcReflect.IsSlice(in); err != nil {
			return err
		}

		valueOf := reflect.ValueOf(in)

		for i := 0; i < valueOf.Len(); i++ {
			msg := grpcReflect.PtrValue(valueOf.Index(i).Interface())

			if err := stream.SendMsg(msg); err != nil {
				return fmt.Errorf("could not send msg: %w", err)
			}
		}

		return nil
	}
}

// RecvAll reads everything from the stream and put into the output.
func RecvAll(out interface{}) ClientStreamHandler {
	return func(stream grpc.ClientStream) error {
		outType, err := isPtrOfSlice(out)
		if err != nil {
			return err
		}

		newOut := reflect.MakeSlice(outType, 0, 0)

		newOut, err = receiveMsg(stream, newOut, outType.Elem())
		if err != nil {
			return err
		}

		reflect.ValueOf(out).Elem().Set(newOut)

		return nil
	}
}

func newSliceMessageValue(t reflect.Type, v reflect.Value) reflect.Value {
	if t.Kind() != reflect.Ptr {
		return v
	}

	result := reflect.New(t.Elem())

	result.Elem().Set(newSliceMessageValue(t.Elem(), v))

	return result
}

func appendMessage(s reflect.Value, v interface{}) reflect.Value {
	return reflect.Append(s, newSliceMessageValue(s.Type().Elem(), grpcReflect.UnwrapValue(v)))
}

func receiveMsg(stream grpc.ClientStream, out reflect.Value, msgType reflect.Type) (reflect.Value, error) {
	for {
		msg := grpcReflect.New(msgType)
		err := stream.RecvMsg(msg)

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return reflect.Value{}, fmt.Errorf("could not recv msg: %w", err)
		}

		out = appendMessage(out, msg)
	}

	return out, nil
}

func isPtrOfSlice(v interface{}) (reflect.Type, error) {
	typeOfPtr := reflect.TypeOf(v)

	if typeOfPtr == nil || typeOfPtr.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("%T is not a pointer", v) // nolint: goerr113
	}

	typeOfSlice := typeOfPtr.Elem()

	if typeOfSlice.Kind() != reflect.Slice {
		return nil, fmt.Errorf("%T is not a slice", v) // nolint: goerr113
	}

	return typeOfSlice, nil
}
