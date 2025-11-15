package main

import (
	// "context"
	"fmt"
	"log"
	"net"

	"golang.org/x/crypto/ssh"

	// "net"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"
)

func main() {
	// get arguments
	arg_list := os.Args[0:]
	if len(arg_list) < 2 {
		log.Fatal("Must have 2 or more arguments.")
	}
	// if len(arg_list) < 3 {
	// 	log.Fatal("Must have 3 or more arguments.")
	// }

	host_cmd := ""
	port := "22"
	privateKeyPath := "/home/ktoks/.ssh/id_ed25519"

	// first argument is the name of the local command
	local_command := arg_list[0]

	// select the key to connect to the right server
	if strings.Contains(local_command, "mcpi") {
		host_cmd = "mcpi"
		// port = "22"
	}
	if strings.Contains(local_command, "ftb") {
		host_cmd = "ftb"
		// port = "22"
	}
	// using the matched host - determine if master is open to matched machine
	socketPath := "/tmp/" + host_cmd + ".sock"
	_, fileErr := os.Stat(socketPath)

	var dialErr error
	var connection net.Conn

	if fileErr == nil {
		connection, dialErr = net.Dial("unix", socketPath)
		if dialErr != nil {
			if err := os.Remove(socketPath); err != nil {
				log.Fatalf("Error removing existing socket: %v", err)
			}
		}
	}

	if fileErr != nil || dialErr != nil {
		signer := getSigner(privateKeyPath)
		user := getUser()
		config := getConfig(user.Username, signer)

		// make initial connection
		client, err := ssh.Dial("tcp", net.JoinHostPort(host_cmd, port), config)
		if err != nil {
			log.Fatalf("Unable to dial new master: %v", err)
		}
		defer client.Close()

		session, err := client.NewSession()
		if err != nil {
			log.Fatalf("Unable to create new session: %v", err)
		}
		defer session.Close()

		// all remote arguments including the remote command
		remote_cmd_str := strings.Join(arg_list[1:], " ")

		output, err := session.CombinedOutput(remote_cmd_str)
		if err != nil {
			log.Fatalf("Command failed: %v", err)
		}
		fmt.Printf("\n%s\n", output)

		// connect socket???
		address := net.UnixAddr{
			Name: socketPath,
			Net:  "unix",
		}

		unixListener, err := net.ListenUnix("unix", &address)
		if err != nil {
			log.Fatalf("Failed to create new Unix Socket: %v", err)
		}
		defer unixListener.Close()

		fmt.Printf("\nNew Unix connection made: %s\n", unixListener.Addr())
		// check the connection -
		time.Sleep(10 * time.Second)
		//connect:
		connection, err = net.Dial("unix", socketPath)
		if err != nil {
			log.Fatalf("Failed to dial through Unix Socket: %v", err)
		}
		fmt.Printf("Server listening on Unix socket: %s\n", socketPath)

		// Handle cleanup on interrupt signals
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			fmt.Println("\nServer shutting down...")
			os.Remove(socketPath) // Remove the socket file
			os.Exit(0)
		}()

		for {
			// Accept incoming connections
			conn, err := unixListener.Accept()
			if err != nil {
				log.Printf("Error accepting connection: %v", err)
				continue
			}
			go handleConnection(conn)
		}
	}

	toprint := connection.LocalAddr().String()
	fmt.Printf("unix connection made:\n%s\n", toprint)

	// fmt.Println("SSH master channel appears up.")
	// fmt.Printf("\n%s\n", output)
	// select {
	// case <-ctx.Done():
	// 	fmt.Println("SSH master channel check timed out.")
	// case err := <-checkMasterChannel(client):
	// 	if err != nil {
	// 		fmt.Printf("SSH master channel appears down: %v\n", err)
	// 	} else {

	// 		session, err := client.NewSession()
	// 		if err != nil {
	// 			log.Fatalf("Unable to create session: %v", err)
	// 		}
	// 		defer session.Close()

	// 		// all remote arguments including the remote command
	// 		remote_cmd_str := strings.Join(arg_list[1:], " ")

	// 		output, err := session.CombinedOutput(remote_cmd_str)
	// 		if err != nil {
	// 	}
	// 		// fmt.Println("SSH master channel appears up.")
	// 	fmt.Printf("\n%s\n", output)
	// config := getConfig(user.name, signer)
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	fmt.Printf("Accepted connection from: %s\n", conn.RemoteAddr())

	buffer := make([]byte, 1024)
	for {
		// Read data from the client
		n, err := conn.Read(buffer)
		if err != nil {
			log.Printf("Error reading from connection: %v", err)
			return
		}

		receivedData := buffer[:n]
		fmt.Printf("Received: %s\n", string(receivedData))

		// Echo the data back to the client
		_, err = conn.Write(receivedData)
		if err != nil {
			log.Printf("Error writing to connection: %v", err)
			return
		}
	}
}

func getConfig(username string, signer ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
}

func getUser() *user.User {
	user, err := user.Current()
	if err != nil {
		log.Fatalf("Unable to get current user: %v", err)
	}
	return user
}

func getSigner(privateKeyPath string) ssh.Signer {
	key, err := os.ReadFile(privateKeyPath)
	if err != nil {
		log.Fatalf("Unable to open private key: %v", err)
	}

	// parse private key
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatalf("Unable to parse private key: %v", err)
	}
	return signer
}

// func checkMasterChannel(client *ssh.Client) chan error {
// 	errChan := make(chan error, 1)
// 	go func() {
// 		// Attempt to create a new session or send a simple command
// 		// This will fail if the underlying connection (master channel) is down
// 		session, err := client.NewSession()
// 		if err != nil {
// 			errChan <- fmt.Errorf("failed to create SSH session: %w", err)
// 			return
// 		}
// 		defer session.Close()

// 		// Execute a simple command to verify connectivity
// 		_, err = session.CombinedOutput("echo hello")
// 		if err != nil {
// 			errChan <- fmt.Errorf("failed to execute command on SSH session: %w", err)
// 			return
// 		}
// 		errChan <- nil // No error, master channel appears up
// 	}()
// 	return errChan
// }
