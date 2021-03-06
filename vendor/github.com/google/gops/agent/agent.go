// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package agent provides hooks programs can register to retrieve
// diagnostics data by using gops.
package agent

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	gosignal "os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"time"

	"github.com/google/gops/internal"
	"github.com/google/gops/signal"
)

const defaultAddr = "127.0.0.1:0"

var (
	mu       sync.Mutex
	portfile string
	listener net.Listener
)

// Options allows configuring the started agent.
type Options struct {
	// Addr is the host:port the agent will be listening at.
	// Optional.
	Addr string

	// NoShutdownCleanup tells the agent not to automatically cleanup
	// resources if the running process recieves an interrupt.
	// Optional.
	NoShutdownCleanup bool
}

// Listen starts the gops agent on a host process. Once agent started, users
// can use the advanced gops features. The agent will listen to Interrupt
// signals and exit the process, if you need to perform further work on the
// Interrupt signal use the options parameter to configure the agent
// accordingly.
//
// Note: The agent exposes an endpoint via a TCP connection that can be used by
// any program on the system. Review your security requirements before starting
// the agent.
func Listen(opts *Options) error {
	mu.Lock()
	defer mu.Unlock()

	if opts == nil {
		opts = &Options{}
	}
	if portfile != "" {
		return fmt.Errorf("gops: agent already listening at: %v", listener.Addr())
	}

	gopsdir, err := internal.ConfigDir()
	if err != nil {
		return err
	}
	err = os.MkdirAll(gopsdir, os.ModePerm)
	if err != nil {
		return err
	}
	if !opts.NoShutdownCleanup {
		gracefulShutdown()
	}

	addr := opts.Addr
	if addr == "" {
		addr = defaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	listener = ln
	port := listener.Addr().(*net.TCPAddr).Port
	portfile = fmt.Sprintf("%s/%d", gopsdir, os.Getpid())
	err = ioutil.WriteFile(portfile, []byte(strconv.Itoa(port)), os.ModePerm)
	if err != nil {
		return err
	}

	go listen()
	return nil
}

func listen() {
	buf := make([]byte, 1)
	for {
		fd, err := listener.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "gops: %v", err)
			if netErr, ok := err.(net.Error); ok && !netErr.Temporary() {
				break
			}
			continue
		}
		if _, err := fd.Read(buf); err != nil {
			fmt.Fprintf(os.Stderr, "gops: %v", err)
			continue
		}
		if err := handle(fd, buf); err != nil {
			fmt.Fprintf(os.Stderr, "gops: %v", err)
			continue
		}
		fd.Close()
	}
}

func gracefulShutdown() {
	c := make(chan os.Signal, 1)
	gosignal.Notify(c, os.Interrupt)
	go func() {
		// cleanup the socket on shutdown.
		<-c
		Close()
		os.Exit(1)
	}()
}

// Close closes the agent, removing temporary files and closing the TCP listener.
// If no agent is listening, Close does nothing.
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if portfile != "" {
		os.Remove(portfile)
		portfile = ""
	}
	if listener != nil {
		listener.Close()
	}
}

func handle(conn net.Conn, msg []byte) error {
	switch msg[0] {
	case signal.StackTrace:
		return pprof.Lookup("goroutine").WriteTo(conn, 2)
	case signal.GC:
		runtime.GC()
		_, err := conn.Write([]byte("ok"))
		return err
	case signal.MemStats:
		var s runtime.MemStats
		runtime.ReadMemStats(&s)
		fmt.Fprintf(conn, "alloc: %v bytes\n", s.Alloc)
		fmt.Fprintf(conn, "total-alloc: %v bytes\n", s.TotalAlloc)
		fmt.Fprintf(conn, "sys: %v bytes\n", s.Sys)
		fmt.Fprintf(conn, "lookups: %v\n", s.Lookups)
		fmt.Fprintf(conn, "mallocs: %v\n", s.Mallocs)
		fmt.Fprintf(conn, "frees: %v\n", s.Frees)
		fmt.Fprintf(conn, "heap-alloc: %v bytes\n", s.HeapAlloc)
		fmt.Fprintf(conn, "heap-sys: %v bytes\n", s.HeapSys)
		fmt.Fprintf(conn, "heap-idle: %v bytes\n", s.HeapIdle)
		fmt.Fprintf(conn, "heap-in-use: %v bytes\n", s.HeapInuse)
		fmt.Fprintf(conn, "heap-released: %v bytes\n", s.HeapReleased)
		fmt.Fprintf(conn, "heap-objects: %v\n", s.HeapObjects)
		fmt.Fprintf(conn, "stack-in-use: %v bytes\n", s.StackInuse)
		fmt.Fprintf(conn, "stack-sys: %v bytes\n", s.StackSys)
		fmt.Fprintf(conn, "next-gc: when heap-alloc >= %v bytes\n", s.NextGC)
		fmt.Fprintf(conn, "last-gc: %v ns\n", s.LastGC)
		fmt.Fprintf(conn, "gc-pause: %v ns\n", s.PauseTotalNs)
		fmt.Fprintf(conn, "num-gc: %v\n", s.NumGC)
		fmt.Fprintf(conn, "enable-gc: %v\n", s.EnableGC)
		fmt.Fprintf(conn, "debug-gc: %v\n", s.DebugGC)
	case signal.Version:
		fmt.Fprintf(conn, "%v\n", runtime.Version())
	case signal.HeapProfile:
		pprof.WriteHeapProfile(conn)
	case signal.CPUProfile:
		if err := pprof.StartCPUProfile(conn); err != nil {
			return err
		}
		time.Sleep(30 * time.Second)
		pprof.StopCPUProfile()
	case signal.Stats:
		fmt.Fprintf(conn, "goroutines: %v\n", runtime.NumGoroutine())
		fmt.Fprintf(conn, "OS threads: %v\n", pprof.Lookup("threadcreate").Count())
		fmt.Fprintf(conn, "GOMAXPROCS: %v\n", runtime.GOMAXPROCS(0))
		fmt.Fprintf(conn, "num CPU: %v\n", runtime.NumCPU())
	}
	return nil
}
