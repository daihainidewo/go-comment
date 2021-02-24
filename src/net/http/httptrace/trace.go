// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package httptrace provides mechanisms to trace the events within
// HTTP client requests.
package httptrace

import (
	"context"
	"crypto/tls"
	"internal/nettrace"
	"net"
	"net/textproto"
	"reflect"
	"time"
)

// unique type to prevent assignment.
type clientEventContextKey struct{}

// ContextClientTrace 返回与ctx相关联的 ClientTrace
// ContextClientTrace returns the ClientTrace associated with the
// provided context. If none, it returns nil.
func ContextClientTrace(ctx context.Context) *ClientTrace {
	trace, _ := ctx.Value(clientEventContextKey{}).(*ClientTrace)
	return trace
}

// WithClientTrace
// WithClientTrace returns a new context based on the provided parent
// ctx. HTTP client requests made with the returned context will use
// the provided trace hooks, in addition to any previous hooks
// registered with ctx. Any hooks defined in the provided trace will
// be called first.
func WithClientTrace(ctx context.Context, trace *ClientTrace) context.Context {
	if trace == nil {
		panic("nil trace")
	}
	old := ContextClientTrace(ctx)
	trace.compose(old)

	ctx = context.WithValue(ctx, clientEventContextKey{}, trace)
	if trace.hasNetHooks() {
		// 有网络hook就将hook存进 nt 中
		nt := &nettrace.Trace{
			ConnectStart: trace.ConnectStart,
			ConnectDone:  trace.ConnectDone,
		}
		if trace.DNSStart != nil {
			nt.DNSStart = func(name string) {
				trace.DNSStart(DNSStartInfo{Host: name})
			}
		}
		if trace.DNSDone != nil {
			nt.DNSDone = func(netIPs []interface{}, coalesced bool, err error) {
				addrs := make([]net.IPAddr, len(netIPs))
				for i, ip := range netIPs {
					addrs[i] = ip.(net.IPAddr)
				}
				trace.DNSDone(DNSDoneInfo{
					Addrs:     addrs,
					Coalesced: coalesced,
					Err:       err,
				})
			}
		}
		ctx = context.WithValue(ctx, nettrace.TraceKey{}, nt)
	}
	return ctx
}

// ClientTrace http发送请求hook合集
// 只针对单次请求 没有支持重定向的hook
// ClientTrace is a set of hooks to run at various stages of an outgoing
// HTTP request. Any particular hook may be nil. Functions may be
// called concurrently from different goroutines and some may be called
// after the request has completed or failed.
//
// ClientTrace currently traces a single HTTP request & response
// during a single round trip and has no hooks that span a series
// of redirected requests.
//
// See https://blog.golang.org/http-tracing for more.
type ClientTrace struct {
	// GetConn 在创建连接或者从空闲连接池中获取连接时调用
	// GetConn is called before a connection is created or
	// retrieved from an idle pool. The hostPort is the
	// "host:port" of the target or proxy. GetConn is called even
	// if there's already an idle cached connection available.
	GetConn func(hostPort string)

	// GotConn 在成功获取连接后调用
	// GotConn is called after a successful connection is
	// obtained. There is no hook for failure to obtain a
	// connection; instead, use the error from
	// Transport.RoundTrip.
	GotConn func(GotConnInfo)

	// PutIdleConn 当连接存放至连接池后调用
	// 如果err为空则存放成功 不为空则描述为啥错误
	// 如果设置了 Transport.DisableKeepAlives 则不会调用
	// PutIdleConn 会在调用 Response.Body.Close 之前返回
	// http2 该hook不会立即执行
	// PutIdleConn is called when the connection is returned to
	// the idle pool. If err is nil, the connection was
	// successfully returned to the idle pool. If err is non-nil,
	// it describes why not. PutIdleConn is not called if
	// connection reuse is disabled via Transport.DisableKeepAlives.
	// PutIdleConn is called before the caller's Response.Body.Close
	// call returns.
	// For HTTP/2, this hook is not currently used.
	PutIdleConn func(err error)

	// GotFirstResponseByte 将相应的header第一个字节可用时调用
	// GotFirstResponseByte is called when the first byte of the response
	// headers is available.
	GotFirstResponseByte func()

	// Got100Continue 如果相应是100则调用
	// Got100Continue is called if the server replies with a "100
	// Continue" response.
	Got100Continue func()

	// Got1xxResponse 在第一个非1xx的响应到来前 每个1xx响应都会调用
	// Got1xxResponse is called for each 1xx informational response header
	// returned before the final non-1xx response. Got1xxResponse is called
	// for "100 Continue" responses, even if Got100Continue is also defined.
	// If it returns an error, the client request is aborted with that error value.
	Got1xxResponse func(code int, header textproto.MIMEHeader) error

	// DNSStart DNS开始查询时调用
	// DNSStart is called when a DNS lookup begins.
	DNSStart func(DNSStartInfo)

	// DNSDone DNS查询结束后调用
	// DNSDone is called when a DNS lookup ends.
	DNSDone func(DNSDoneInfo)

	// ConnectStart 开始新建连接时调用
	// ConnectStart is called when a new connection's Dial begins.
	// If net.Dialer.DualStack (IPv6 "Happy Eyeballs") support is
	// enabled, this may be called multiple times.
	ConnectStart func(network, addr string)

	// ConnectDone 新建连接后调用
	// ConnectDone is called when a new connection's Dial
	// completes. The provided err indicates whether the
	// connection completedly successfully.
	// If net.Dialer.DualStack ("Happy Eyeballs") support is
	// enabled, this may be called multiple times.
	ConnectDone func(network, addr string, err error)

	// TLSHandshakeStart TLS握手开始前调用
	// TLSHandshakeStart is called when the TLS handshake is started. When
	// connecting to an HTTPS site via an HTTP proxy, the handshake happens
	// after the CONNECT request is processed by the proxy.
	TLSHandshakeStart func()

	// TLSHandshakeDone TLS握手结束后调用
	// TLSHandshakeDone is called after the TLS handshake with either the
	// successful handshake's connection state, or a non-nil error on handshake
	// failure.
	TLSHandshakeDone func(tls.ConnectionState, error)

	// WroteHeaderField 在写入Header后调用
	// WroteHeaderField is called after the Transport has written
	// each request header. At the time of this call the values
	// might be buffered and not yet written to the network.
	WroteHeaderField func(key string, value []string)

	// WroteHeaders 写完所有的Header之后调用
	// WroteHeaders is called after the Transport has written
	// all request headers.
	WroteHeaders func()

	// Wait100Continue 在写入请求时在等待100时调用
	// Wait100Continue is called if the Request specified
	// "Expect: 100-continue" and the Transport has written the
	// request headers but is waiting for "100 Continue" from the
	// server before writing the request body.
	Wait100Continue func()

	// WroteRequest 写入请求后调用 可能在重试的时候调用多次
	// WroteRequest is called with the result of writing the
	// request and any body. It may be called multiple times
	// in the case of retried requests.
	WroteRequest func(WroteRequestInfo)
}

// WroteRequestInfo 包含写入请求hook
// WroteRequestInfo contains information provided to the WroteRequest
// hook.
type WroteRequestInfo struct {
	// Err is any error encountered while writing the Request.
	Err error
}

// compose 将t的成员函数变量设置成old的成员函数变量
// 如果t的函数不为空则先执行后再设置成old的成员函数变量
// compose modifies t such that it respects the previously-registered hooks in old,
// subject to the composition policy requested in t.Compose.
func (t *ClientTrace) compose(old *ClientTrace) {
	if old == nil {
		return
	}
	tv := reflect.ValueOf(t).Elem()
	ov := reflect.ValueOf(old).Elem()
	structType := tv.Type()
	for i := 0; i < structType.NumField(); i++ {
		tf := tv.Field(i)
		hookType := tf.Type()
		if hookType.Kind() != reflect.Func {
			continue
		}
		of := ov.Field(i)
		if of.IsNil() {
			continue
		}
		if tf.IsNil() {
			tf.Set(of)
			continue
		}

		// Make a copy of tf for tf to call. (Otherwise it
		// creates a recursive call cycle and stack overflows)
		tfCopy := reflect.ValueOf(tf.Interface())

		// We need to call both tf and of in some order.
		newFunc := reflect.MakeFunc(hookType, func(args []reflect.Value) []reflect.Value {
			tfCopy.Call(args)
			return of.Call(args)
		})
		tv.Field(i).Set(newFunc)
	}
}

// DNSStartInfo 包含DNS请求信息
// DNSStartInfo contains information about a DNS request.
type DNSStartInfo struct {
	Host string
}

// DNSDoneInfo 包含DNS查询记录信息
// DNSDoneInfo contains information about the results of a DNS lookup.
type DNSDoneInfo struct {
	// Addrs 表示查询的IP地址结果
	// Addrs are the IPv4 and/or IPv6 addresses found in the DNS
	// lookup. The contents of the slice should not be mutated.
	Addrs []net.IPAddr

	// Err 在DNS查询过程中发生错误
	// Err is any error that occurred during the DNS lookup.
	Err error

	// Coalesced 是否和其他同时进行相同DNS查询的调用者共享结果
	// Coalesced is whether the Addrs were shared with another
	// caller who was doing the same DNS lookup concurrently.
	Coalesced bool
}

func (t *ClientTrace) hasNetHooks() bool {
	if t == nil {
		return false
	}
	return t.DNSStart != nil || t.DNSDone != nil || t.ConnectStart != nil || t.ConnectDone != nil
}

// GotConnInfo 作为 ClientTrace.GotConn 的参数 包含连接的一些信息
// GotConnInfo is the argument to the ClientTrace.GotConn function and
// contains information about the obtained connection.
type GotConnInfo struct {
	// Conn http.Transport 的连接 这里不应对连接进行读写关闭操作
	// Conn is the connection that was obtained. It is owned by
	// the http.Transport and should not be read, written or
	// closed by users of ClientTrace.
	Conn net.Conn

	// Reused 表示此条连接是否在之前已经完成过http请求
	// Reused is whether this connection has been previously
	// used for another HTTP request.
	Reused bool

	// WasIdle 表示词条连接是否是从空闲连接列表获取
	// WasIdle is whether this connection was obtained from an
	// idle pool.
	WasIdle bool

	// IdleTime 如果 WasIdle 为 true 则记录空闲时长
	// IdleTime reports how long the connection was previously
	// idle, if WasIdle is true.
	IdleTime time.Duration
}
