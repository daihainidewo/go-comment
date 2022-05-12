// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements sysSocket for platforms that provide a fast path for
// setting SetNonblock and CloseOnExec.

//go:build dragonfly || freebsd || linux || netbsd || openbsd || solaris

package net

import (
	"os"
	"syscall"
)

// sysSocket 返回系统文件描述符 标记为非阻塞且close-on-exec
// 如果创建非阻塞且close-on-exec 获取指定失败错误码时 尝试使用系统调用的方式设置上去
// Wrapper around the socket system call that marks the returned file
// descriptor as nonblocking and close-on-exec.
func sysSocket(family, sotype, proto int) (int, error) {
	// 通过系统调用创建套接字
	s, err := socketFunc(family, sotype|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, proto)
	if err != nil {
		return -1, os.NewSyscallError("socket", err)
	}
	return s, nil
}
