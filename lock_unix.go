//go:build !windows

package main

import (
	"fmt"
	"log/syslog"
	"os"
	"syscall"
)

func acquireLockOrExit() {
	lockFile, err := os.OpenFile("/tmp/streamer.lock", os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		fmt.Printf("Failed to open lock file: %v\n", err)
		os.Exit(1)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Streamer is already running. Exiting.")
		if sl, err := syslog.New(syslog.LOG_WARNING|syslog.LOG_DAEMON, "streamer"); err == nil {
			sl.Warning("Streamer is already running. Exiting.")
			sl.Close()
		}
		os.Exit(1)
	}
}
