package main

import (
	// "errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func main() {
	// get arguments
	arg_list := os.Args[0:]
	if len(arg_list) < 2 {
		log.Fatal("Must have 2 or more records.")
	}
	// if len(arg_list) < 3 {
	// 	log.Fatal("Must have 3 or more records.")
	// }

	host_cmd := ""

	// first argument is the name of the local command
	local_command := filepath.Base(arg_list[0])
	// select the key to connect to the right server
	fmt.Println("Local command invoked: ", local_command)

	// check for string that matches a server name
	if strings.Contains(local_command, "mcpi") {
		host_cmd = "mcpi"
	}
	if strings.Contains(local_command, "ftb") {
		host_cmd = "ftb"
	}

	var dialErr error
	var conn net.Conn
	// var connection net.Conn
	// socketPath := "/tmp/" + host_cmd + ".sock"

	user := getUser()
	// // Check for master connection
	// _, err := os.Stat(socketPath)
	socketPath := user.HomeDir + "/.ssh/sockets/" + user.Username + "@" + host_cmd + ":22.sock"
	_, err := os.Stat(socketPath)

	// socket found - try to dial
	if err == nil {
		conn, dialErr = net.Dial("unix", socketPath)

		// if dial fails, delete the file
		if dialErr != nil {
			remErr := os.Remove(socketPath)
			if remErr != nil {
				log.Fatalf("Error removing existing socket: %v", remErr)
			}
		}
	}

	// if file doesn't exist or connection fails, load new master
	if err != nil {
		// get private key
		privateKeyPath := "/home/ktoks/.ssh/id_ed25519"
		key, err := os.ReadFile(privateKeyPath)
		if err != nil {
			log.Fatalf("Unable to read private key: %v", err)
		}

		// parse private key
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			log.Fatalf("Unable to parse private key: %v", err)
		}

		cmd := exec.Command("ssh", "-N", "-i", "<<<"+string(signer.PublicKey().Marshal()), host_cmd)
		err = cmd.Start()
		time.Sleep(time.Second)

		if err != nil {
			os.Remove(socketPath)
			log.Fatalf("Error, failed to start new master: %v", err)
		}

		// Dial the new SSH agent socket created for the master
		conn, err = net.Dial("unix", socketPath)
		if err != nil {
			log.Fatalf("Failed to open SSH_AUTH_SOCK: %v", err)
		}
	}

	// get private key
	// Create a new SSH agent client.
	agentClient := agent.NewClient(conn)

	// Configure the SSH client to use agent-based authentication.
	config := &ssh.ClientConfig{
		User: user.Username, // Replace with your username
		Auth: []ssh.AuthMethod{
			ssh.PublicKeysCallback(agentClient.Signers),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // For demonstration, handle properly in production
	}

	// // get ssh config
	// config := &ssh.ClientConfig{
	// 	User: "ktoks",
	// 	Auth: []ssh.AuthMethod{
	// 		ssh.PublicKeys(signer),
	// 	},
	// 	HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	// }

	addr := host_cmd + ":22"
	fmt.Println("host:port: ", addr)

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		log.Fatalf("unable to connect: %v", err)
	}
	defer client.Close()

	// all remote arguments including the remote command
	remote_cmd_str := strings.Join(arg_list[1:], " ")

	session, err := client.NewSession()
	if err != nil {
		log.Fatalf("unable to create session: %v", err)
	}
	defer session.Close()

	fmt.Println("remote cmd: ", remote_cmd_str)
	output, err := session.CombinedOutput(remote_cmd_str)
	if err != nil {
		log.Fatalf("Command failed: %v", err)
	}
	fmt.Printf("\n%s\n", output)
}

func getUser() *user.User {
	user, err := user.Current()
	if err != nil {
		log.Fatalf("Unable to get current user: %v", err)
	}
	return user
}
