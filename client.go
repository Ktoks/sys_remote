package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall" // Required for os/exec.Cmd.SysProcAttr struct definition
	"time"

	"golang.org/x/sys/unix" // Replaces syscall for actual logic (locking)
)

func connectToMaster(socketPath, linkName string) (net.Conn, error) {
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		if err := startMasterProcess(linkName); err != nil {
			return nil, fmt.Errorf("failed to spawn master: %v", err)
		}
		for range 20 {
			time.Sleep(200 * time.Millisecond)
			connection, err = net.Dial("unix", socketPath)
			if err == nil {
				return connection, nil
			}
		}
		return nil, fmt.Errorf("timeout waiting for master")
	}
	// Successful connection, no error
	return connection, nil
}

func startMasterProcess(identity string) error {
	homeDir, _ := os.UserHomeDir()
	socketDir := filepath.Join(homeDir, SocketDir)

	// ensure the directory exists
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		return fmt.Errorf("failed to create socket dir: %v", err)
	}

	lockPath := filepath.Join(socketDir, identity+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}

	defer func() {
		err = lockFile.Close()
		if err != nil {
			fmt.Println("listener close error: ", err)
		}
	}()

	// UPDATED: Use unix package for locking
	// Try to acquire an exclusive lock
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		// UPDATED: Use unix package for unlocking
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		if err != nil {
			fmt.Println("error getting lock: ", err)
		}
	}()

	// CHECK: After acquiring lock, check if socket exists now.
	socketPath := getSocketPath(homeDir, identity)
	if _, err := os.Stat(socketPath); err == nil {
		// socket exists now, no need to spawn
		return nil
	}

	selfExe, err := os.Executable()
	if err != nil {
		return err
	}

	logPath := filepath.Join(socketDir, identity+".log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to create daemon log: %v", err)
	}

	cmd := exec.Command(selfExe, "--daemon", identity)
	cmd.Env = os.Environ()

	// Note: We must still use syscall.SysProcAttr here because
	// os/exec expects this specific struct type.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		log_err := logFile.Close()
		if log_err != nil {
			fmt.Println("log file close error: ", log_err)
		}
		return err
	}
	log_err := logFile.Close()
	if log_err != nil {
		fmt.Println("log file close error: ", log_err)
	}
	return nil
}

func runBatchMode(connection net.Conn) {
	// 1. Sender Routine (Async)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			cmd := scanner.Text()
			if strings.TrimSpace(cmd) == "" {
				continue
			}
			// Write command to socket
			_, err := fmt.Fprintf(connection, "%s\n", cmd)
			if err != nil {
				return
			}
		}
		// Close write end to signal EOF
		if unixConn, ok := connection.(*net.UnixConn); ok {
			err := unixConn.CloseWrite()
			if err != nil {
				fmt.Println("connection error: ", err)
			}
		} else if tcpConn, ok := connection.(*net.TCPConn); ok {
			err := tcpConn.CloseWrite()
			if err != nil {
				fmt.Println("connection error: ", err)
			}
		}
	}()

	// 2. Receiver Routine
	processIncomingPackets(connection)
}

func sendCommand(connection net.Conn, cmd string) error {
	_, err := fmt.Fprintf(connection, "%s\n", cmd)
	if err != nil {
		return err
	}
	processIncomingPackets(connection)
	return nil
}
