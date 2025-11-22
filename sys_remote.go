package main

import (
	"bufio"
	"encoding/binary" // <--- NEW: For fast binary I/O
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Configuration Constants
const (
	RemotePort  = "22"
	SocketDir   = ".ssh/sockets"
	IdleTimeout = 5 * time.Minute
)

// Protocol Constants (Packet Types)
const (
	TypeStdout = 0x01
	TypeStderr = 0x02
	TypeExit   = 0x03
)

// OutputPacket defines the internal structure (not sent over wire directly anymore)
type OutputPacket struct {
	IsStderr bool
	IsExit   bool
	ExitCode int
	Data     []byte
}

// SafeEncoder: Replaces Gob with manual binary writing
// It ensures that multiple threads writing to the socket don't interleave their bytes.
type SafeEncoder struct {
	mu     sync.Mutex
	writer io.Writer
}

func (s *SafeEncoder) Encode(pkt OutputPacket) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Determine Type and Payload
	var pType uint8
	var payload []byte

	if pkt.IsExit {
		pType = TypeExit
		// Convert ExitCode (int) to 4 bytes
		payload = make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(pkt.ExitCode))
	} else if pkt.IsStderr {
		pType = TypeStderr
		payload = pkt.Data
	} else {
		pType = TypeStdout
		payload = pkt.Data
	}

	// 2. Write Header [Type (1) + Length (4)]
	header := make([]byte, 5)
	header[0] = pType
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))

	if _, err := s.writer.Write(header); err != nil {
		return err
	}

	// 3. Write Payload
	if len(payload) > 0 {
		if _, err := s.writer.Write(payload); err != nil {
			return err
		}
	}

	return nil
}

// Flags
var daemonIdentity = flag.String("daemon", "", "Internal use: run as daemon for specific link identity")
var batchMode = flag.Bool("batch", false, "Run in batch mode (reads commands from stdin)")

func main() {
	flag.Parse()

	userHome, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Error getting home dir: %v", err)
	}

	// 1. MASTER MODE
	if *daemonIdentity != "" {
		linkName := *daemonIdentity
		runMaster(resolveHost(linkName), getSocketPath(userHome, linkName), userHome)
		return
	}

	// 2. CLIENT MODE
	linkName := filepath.Base(os.Args[0])
	socketPath := getSocketPath(userHome, linkName)

	conn, err := connectToMaster(socketPath, linkName)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// A. BATCH MODE (High Performance)
	if *batchMode {
		runBatchMode(conn)
		return
	}

	// B. STANDARD MODE (Single Command)
	args := flag.Args()
	if len(args) == 0 {
		fmt.Printf("Usage: %s <command> OR %s --batch\n", linkName, linkName)
		os.Exit(1)
	}

	remoteCmd := strings.Join(args, " ")

	if err := sendCommand(conn, remoteCmd); err != nil {
		log.Fatal(err)
	}
}

// --- CLIENT LOGIC ---

func runBatchMode(conn net.Conn) {
	// 1. Sender Routine (Async)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			cmd := scanner.Text()
			if strings.TrimSpace(cmd) == "" {
				continue
			}
			// Write command to socket
			_, err := fmt.Fprintf(conn, "%s\n", cmd)
			if err != nil {
				return
			}
		}
		// Close write end to signal EOF
		if unixConn, ok := conn.(*net.UnixConn); ok {
			unixConn.CloseWrite()
		} else if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()

	// 2. Receiver Routine
	processIncomingPackets(conn)
}

func sendCommand(conn net.Conn, cmd string) error {
	_, err := fmt.Fprintf(conn, "%s\n", cmd)
	if err != nil {
		return err
	}
	processIncomingPackets(conn)
	return nil
}

// processIncomingPackets: The unified binary decoder loop
func processIncomingPackets(conn io.Reader) {
	header := make([]byte, 5) // [Type:1][Len:4]

	for {
		// 1. Read Header
		_, err := io.ReadFull(conn, header)
		if err != nil {
			if err == io.EOF {
				break // Master closed connection
			}
			log.Printf("Protocol error (reading header): %v", err)
			break
		}

		pType := header[0]
		pLen := binary.BigEndian.Uint32(header[1:])

		// 2. Read Payload
		payload := make([]byte, pLen)
		if pLen > 0 {
			_, err := io.ReadFull(conn, payload)
			if err != nil {
				log.Printf("Protocol error (reading payload): %v", err)
				break
			}
		}

		// 3. Handle Data
		switch pType {
		case TypeStdout:
			os.Stdout.Write(payload)
		case TypeStderr:
			os.Stderr.Write(payload)
		case TypeExit:
			exitCode := int(binary.BigEndian.Uint32(payload))

			// --- FIX START ---
			// 1. If we are in SINGLE command mode, we must exit NOW,
			//    regardless of whether success (0) or failure (1+).
			if !*batchMode {
				os.Exit(exitCode)
			}

			// 2. If we are in BATCH mode, we just log errors and keep listening
			if exitCode != 0 {
				fmt.Fprintf(os.Stderr, "[Remote Exit %d]\n", exitCode)
			}
			// --- FIX END ---
		}
	}
}

func connectToMaster(socketPath, linkName string) (net.Conn, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		if err := startMasterProcess(linkName); err != nil {
			return nil, fmt.Errorf("failed to spawn master: %v", err)
		}
		for i := 0; i < 20; i++ {
			time.Sleep(200 * time.Millisecond)
			conn, err = net.Dial("unix", socketPath)
			if err == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("timeout waiting for master")
	}
	return conn, nil
}

// --- MASTER LOGIC ---

func runMaster(host string, socketPath string, homeDir string) {
	log.SetOutput(os.Stderr)
	log.Printf("Daemon starting for host: %s (Optimized Binary Protocol)", host)

	socketDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		log.Fatalf("Daemon failed to create socket dir: %v", err)
	}

	currentUser := os.Getenv("USER")
	client, err := createSSHClient(host, homeDir, currentUser)
	if err != nil {
		log.Fatalf("SSH Handshake Failed: %v", err)
	}
	defer client.Close()
	log.Println("SSH Connection Established.")

	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("Daemon failed to listen on socket: %v", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	var activeConnections int32

	for {
		listener.(*net.UnixListener).SetDeadline(time.Now().Add(IdleTimeout))
		conn, err := listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				if atomic.LoadInt32(&activeConnections) > 0 {
					continue
				}
				log.Println("Idle timeout reached. Shutting down.")
				return
			}
			log.Printf("Accept error: %v", err)
			return
		}

		atomic.AddInt32(&activeConnections, 1)
		go func() {
			defer atomic.AddInt32(&activeConnections, -1)
			handleRequest(conn, client)
		}()
	}
}

func handleRequest(conn net.Conn, client *ssh.Client) {
	defer conn.Close()

	// Initialize the custom binary encoder
	safeEnc := &SafeEncoder{
		writer: conn, // Write directly to the socket
	}
	reader := bufio.NewReader(conn)

	maxConcurrency := 50
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for {
		cmdStr, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		cmdStr = strings.TrimSpace(cmdStr)
		if cmdStr == "" {
			continue
		}

		sem <- struct{}{}
		wg.Add(1)

		go func(cmd string) {
			defer wg.Done()
			defer func() { <-sem }()
			runRemoteCommand(client, cmd, safeEnc)
		}(cmdStr)
	}
	wg.Wait()
}

func runRemoteCommand(client *ssh.Client, cmdStr string, safeEnc *SafeEncoder) {
	session, err := client.NewSession()
	if err != nil {
		// --- FIX: Report internal error to client so it doesn't hang ---
		errMsg := fmt.Sprintf("Daemon error: failed to create SSH session: %v\n", err)
		safeEnc.Encode(OutputPacket{IsStderr: true, Data: []byte(errMsg)})
		safeEnc.Encode(OutputPacket{IsExit: true, ExitCode: 255})
		return
	}

	output, err := session.CombinedOutput(cmdStr)
	session.Close()

	// Send Data (Type 0 or 1)
	if len(output) > 0 {
		safeEnc.Encode(OutputPacket{IsStderr: false, Data: output})
	}

	// Send Exit Code (Type 3)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			exitCode = 1
		}
	}
	safeEnc.Encode(OutputPacket{IsExit: true, ExitCode: exitCode})
}

// --- HELPERS (Unchanged mostly) ---

func getSocketPath(homeDir, linkName string) string {
	return filepath.Join(homeDir, SocketDir, fmt.Sprintf("%s.sock", linkName))
}

func startMasterProcess(identity string) error {
	selfExe, err := os.Executable()
	if err != nil {
		return err
	}

	homeDir, _ := os.UserHomeDir()
	socketDir := filepath.Join(homeDir, SocketDir)
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		return fmt.Errorf("failed to create socket dir: %v", err)
	}

	logPath := filepath.Join(socketDir, identity+".log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to create daemon log: %v", err)
	}

	cmd := exec.Command(selfExe, "--daemon", identity)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	logFile.Close()
	return nil
}

func createSSHClient(host string, home string, user string) (*ssh.Client, error) {
	khPath := filepath.Join(home, ".ssh", "known_hosts")
	hkCallback, err := knownhosts.New(khPath)
	if err != nil {
		log.Printf("Warning: known_hosts not found, using insecure fallback")
		hkCallback = ssh.InsecureIgnoreHostKey()
	}

	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentClient := agent.NewClient(conn)
			if signers, _ := agentClient.Signers(); len(signers) > 0 {
				auths = append(auths, ssh.PublicKeysCallback(agentClient.Signers))
			}
		}
	}

	keyFiles := []string{"id_ed25519", "id_rsa"}
	for _, name := range keyFiles {
		keyPath := filepath.Join(home, ".ssh", name)
		keyBytes, err := os.ReadFile(keyPath)
		if err == nil {
			signer, err := ssh.ParsePrivateKey(keyBytes)
			if err == nil {
				auths = append(auths, ssh.PublicKeys(signer))
			}
		}
	}

	if len(auths) == 0 {
		return nil, fmt.Errorf("no auth methods found")
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hkCallback,
		Timeout:         5 * time.Second,
	}

	addr := net.JoinHostPort(host, RemotePort)
	return ssh.Dial("tcp", addr, config)
}

func resolveHost(linkName string) string {
	if strings.Contains(linkName, "mcpi") {
		return "mcpi"
	}
	if strings.Contains(linkName, "ftb") {
		return "ftb"
	}
	return linkName
}
