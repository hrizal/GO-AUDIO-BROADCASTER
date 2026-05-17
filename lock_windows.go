//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
)

var keepListenerAlive net.Listener

func acquireLockOrExit() {
	// Bind to local port 29126 to ensure only one instance runs on Windows
	listener, err := net.Listen("tcp", "127.0.0.1:29126")
	if err != nil {
		fmt.Println("Streamer is already running (failed to acquire port 29126). Exiting.")
		os.Exit(1)
	}
	keepListenerAlive = listener
}
