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
	// all remote arguments including the remote command
	remote_cmd_str := strings.Join(arg_list[1:], " ")

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

		output, err := session.CombinedOutput(remote_cmd_str)
		if err != nil {
			log.Fatalf("Command failed: %v", err)
		}
		fmt.Printf("\n%s\n", output)

		address := net.UnixAddr{
			Name: socketPath,
			Net:  "unix",
		}

		// start socket
		unixListener, err := net.ListenUnix("unix", &address)
		if err != nil {
			log.Fatalf("Failed to create new Unix Socket: %v", err)
		}
		defer unixListener.Close()
		unixListener.SetUnlinkOnClose(true)

		fmt.Printf("\nNew Unix connection made: %s\n", unixListener.Addr())
		// check the connection -
		// time.Sleep(10 * time.Second)
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
			go handleConnection(conn, remote_cmd_str, client)
		}
	}

	fmt.Printf("Writing remote command...")
	_, err := connection.Write([]byte(remote_cmd_str))
	if err != nil {
		log.Fatalf("Write failed: %v", err)
	}

	err = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		log.Fatalf("Set read deadline failed: %v", err)
	}
	_, err = connection.Read([]byte(remote_cmd_str))
	if err != nil {
		log.Fatalf("Read failed: %v", err)
	}

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

func handleConnection(conn net.Conn, remote_cmd_str string, client *ssh.Client) {
	defer conn.Close()

	fmt.Printf("Waiting for connection on: %v\n", conn.RemoteAddr())

	buffer := make([]byte, 1024)
	for {
		// Read data from the client

		// err := conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		// if err != nil {
		// 	log.Fatalf("Failed to set read deadline: %v", err)
		// 	break
		// }

		n, err := conn.Read(buffer)
		// fmt.Println("connection read...")
		if os.IsTimeout(err) {
			fmt.Println("Timed out, disconnecting")
			break
		} else if err != nil {
			log.Printf("Error reading from connection: %v", err)
		}

		receivedData := buffer[:n]
		fmt.Printf("Received: %s\n", string(receivedData))
		session, err := client.NewSession()
		if err != nil {
			log.Fatalf("New session failed: %v", err)
		}
		output, err := session.CombinedOutput(remote_cmd_str)
		if err != nil {
			log.Fatalf("Remote command failed: %v", err)
		}

		// Echo the data back to the client
		_, err = conn.Write(output)
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
