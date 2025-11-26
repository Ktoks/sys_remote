package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// It ensures that multiple threads writing to the socket don't interleave their bytes.
type SafeEncoder struct {
	mu     sync.Mutex
	writer io.Writer
}

// OutputPacket defines the internal structure (not sent over wire directly)
type OutputPacket struct {
	IsStderr bool
	IsExit   bool
	ExitCode int
	Data     []byte
}

func startMaster(host string, socketPath string, homeDir string) {
	log.SetOutput(os.Stderr)
	log.Printf("*** %s *** daemon starting", host)

	socketDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		log.Fatalf("daemon failed to create socket dir: %v", err)
	}

	currentUser := os.Getenv("USER")
	client, err := createSSHClient(host, homeDir, currentUser)
	if err != nil {
		log.Fatalf("ssh handshake hailed: %v", err)
	}

	defer func() {
		err = client.Close()
		if err != nil {
			log.Println("connection close error: ", err)
		}
	}()
	log.Printf("%s connection established.", host)

	// check, then clean up old socket
	_, err = os.Stat(socketPath)

	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatal("filesystem error: ", err)
		}
	} else {
		log.Println("old socket found, removing...")
		removeErr := os.Remove(socketPath)
		if removeErr != nil {
			log.Fatal("cannot remove socket: ", removeErr)
		}
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("daemon failed to listen on socket: %v", err)
	}

	defer func() {
		err = listener.Close()
		if err != nil {
			log.Println("listener close error: ", err)
		}
	}()

	defer func() {
		err = os.Remove(socketPath)
		if err != nil {
			log.Println("socket cleanup error: ", err)
		}
	}()

	var activeConnections int32

	for {
		setDeadlineErr := listener.(*net.UnixListener).SetDeadline(time.Now().Add(IdleTimeout))
		if setDeadlineErr != nil {
			log.Println("setting deadline failed: ", err)
		}
		connection, err := listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				if atomic.LoadInt32(&activeConnections) > 0 {
					continue
				}
				log.Println("idle timeout reached - shutting down")
				return
			}
			log.Printf("listener accept error: %v", err)
			return
		}

		atomic.AddInt32(&activeConnections, 1)
		go func() {
			defer atomic.AddInt32(&activeConnections, -1)
			handleRequest(connection, client)
		}()
	}
}

func handleRequest(connection net.Conn, client *ssh.Client) {
	err := errors.New("")
	defer func() {
		err = connection.Close()
		if err != nil {
			log.Println("handleRequest: connection close error: ", err)
		}
	}()

	// Initialize the custom binary encoder
	safeEncoder := &SafeEncoder{
		writer: connection, // Write directly to the socket
	}
	reader := bufio.NewReader(connection)

	maxConcurrency := 50
	semaphore := make(chan struct{}, maxConcurrency)
	var waitGroup sync.WaitGroup

	for {
		commandString, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		commandString = strings.TrimSpace(commandString)
		if commandString == "" {
			continue
		}

		semaphore <- struct{}{}
		waitGroup.Add(1)

		go func(command string) {

			defer waitGroup.Done()
			defer func() { <-semaphore }()
			runRemoteCommand(client, command, safeEncoder)
		}(commandString)
	}
	waitGroup.Wait()
}

func runRemoteCommand(client *ssh.Client, commandString string, safeEncoder *SafeEncoder) {
	session, err := client.NewSession()
	if err != nil {
		errMsg := fmt.Sprintf("daemon error: failed to create SSH session: %v\n", err)
		err := safeEncoder.Encode(OutputPacket{IsStderr: true, Data: []byte(errMsg)})
		if err != nil {
			log.Println("encoding STDERR failed: ", err)
		}
		err = safeEncoder.Encode(OutputPacket{IsExit: true, ExitCode: 255})
		if err != nil {
			log.Println("encoding IsExit failed: ", err)
		}
		return
	}

	output, err := session.CombinedOutput(commandString)

	closeErr := session.Close()
	if closeErr != nil {
		if closeErr == io.EOF {
			log.Println("completed processing: ", commandString)
		} else {
			log.Println("session error (closing): ", closeErr)
		}
	}

	// Send Data (Type 0 or 1)
	if len(output) > 0 {
		err := safeEncoder.Encode(OutputPacket{IsStderr: false, Data: output})
		if err != nil {
			log.Println("encoding IsStderr failed: ", err)
		}
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
	err = safeEncoder.Encode(OutputPacket{IsExit: true, ExitCode: exitCode})
	if err != nil {
		log.Println("encoding IsExit failed: ", err)
	}
}

func createSSHClient(host string, home string, user string) (*ssh.Client, error) {
	knownHostPath := filepath.Join(home, ".ssh", "known_hosts")
	hostKeyCallback, err := knownhosts.New(knownHostPath)
	if err != nil {
		log.Printf("warning: known_hosts not found, using insecure fallback")
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	}

	var authList []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if connection, err := net.Dial("unix", sock); err == nil {
			agentClient := agent.NewClient(connection)
			if signers, _ := agentClient.Signers(); len(signers) > 0 {
				authList = append(authList, ssh.PublicKeysCallback(agentClient.Signers))
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
				authList = append(authList, ssh.PublicKeys(signer))
			}
		}
	}

	if len(authList) == 0 {
		return nil, fmt.Errorf("no auth methods found")
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authList,
		HostKeyCallback: hostKeyCallback,
		Timeout:         5 * time.Second,
	}

	return ssh.Dial("tcp", net.JoinHostPort(host, RemotePort), config)
}

// processIncomingPackets: The unified binary decoder loop
func processIncomingPackets(connection io.Reader) {
	header := make([]byte, 5) // [Type:1][Len:4]

	for {
		// 1. Read Header
		_, err := io.ReadFull(connection, header)
		if err != nil {
			if err == io.EOF {
				break // Master closed connection
			}
			log.Printf("protocol error (reading header): %v", err)
			break
		}

		payloadType := header[0]
		payloadLength := binary.BigEndian.Uint32(header[1:])

		// 2. Read Payload
		payload := make([]byte, payloadLength)
		if payloadLength > 0 {
			_, err := io.ReadFull(connection, payload)
			if err != nil {
				log.Printf("protocol error (reading payload): %v", err)
				break
			}
		}

		// 3. Handle Data
		switch payloadType {

		case TypeStdout:
			number, err := os.Stdout.Write(payload)
			if err != nil {
				log.Printf("Stdout write error: %v, %v", err, number)
			}

		case TypeStderr:
			number, err := os.Stderr.Write(payload)
			if err != nil {
				log.Printf("Stderr write error: %v, %v", err, number)
			}

		case TypeExit:
			exitCode := int(binary.BigEndian.Uint32(payload))

			// 1. If we are in SINGLE command mode, we must exit NOW,
			//    regardless of whether success (0) or failure (1+).
			if !*batchMode {
				os.Exit(exitCode)
			}

			// 2. If we are in BATCH mode, we just log errors and keep listening
			if exitCode != 0 {
				fmt.Fprintf(os.Stderr, "[Remote Exit %d]\n", exitCode)
			}
		}
	}
}

func (safeEncoder *SafeEncoder) Encode(outPacket OutputPacket) error {
	safeEncoder.mu.Lock()
	defer safeEncoder.mu.Unlock()

	// 1. Determine Type and Payload
	var payloadType uint8
	var payload []byte

	if outPacket.IsExit {
		payloadType = TypeExit
		// Convert ExitCode (int) to 4 bytes
		payload = make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(outPacket.ExitCode))
	} else if outPacket.IsStderr {
		payloadType = TypeStderr
		payload = outPacket.Data
	} else {
		payloadType = TypeStdout
		payload = outPacket.Data
	}

	// 2. Write Header [Type (1) + Length (4)]
	header := make([]byte, 5)
	header[0] = payloadType
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))

	if _, err := safeEncoder.writer.Write(header); err != nil {
		return err
	}

	// 3. Write Payload
	if len(payload) > 0 {
		if _, err := safeEncoder.writer.Write(payload); err != nil {
			return err
		}
	}

	return nil
}
