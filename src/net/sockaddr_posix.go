// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || js || wasip1 || windows

package net

import (
	"syscall"
)

// sockaddr 基于socket的接口
// A sockaddr represents a TCP, UDP, IP or Unix network endpoint
// address that can be converted into a syscall.Sockaddr.
type sockaddr interface {
	// 端点地址
	Addr

	// 协议族
	// family returns the platform-dependent address family
	// identifier.
	family() int

	// 是否是通配符地址
	// isWildcard reports whether the address is a wildcard
	// address.
	isWildcard() bool

	// 返回系统的sockaddr
	// sockaddr returns the address converted into a syscall
	// sockaddr type that implements syscall.Sockaddr
	// interface. It returns a nil interface when the address is
	// nil.
	sockaddr(family int) (syscall.Sockaddr, error)

	// 将通配符地址映射到环回地址
	// toLocal maps the zero address to a local system address (127.0.0.1 or ::1)
	toLocal(net string) sockaddr
}

func (fd *netFD) addrFunc() func(syscall.Sockaddr) Addr {
	switch fd.family {
	case syscall.AF_INET, syscall.AF_INET6:
		switch fd.sotype {
		case syscall.SOCK_STREAM:
			return sockaddrToTCP
		case syscall.SOCK_DGRAM:
			return sockaddrToUDP
		case syscall.SOCK_RAW:
			return sockaddrToIP
		}
	case syscall.AF_UNIX:
		switch fd.sotype {
		case syscall.SOCK_STREAM:
			return sockaddrToUnix
		case syscall.SOCK_DGRAM:
			return sockaddrToUnixgram
		case syscall.SOCK_SEQPACKET:
			return sockaddrToUnixpacket
		}
	}
	return func(syscall.Sockaddr) Addr { return nil }
}
