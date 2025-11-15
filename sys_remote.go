package main

import (
	"fmt"
	"golang.org/x/crypto/ssh"
	"log"
	"os"
	"strings"
)

func main() {
	// get arguments
	arg_list := os.Args[0:]
	if len(arg_list) < 3 {
		log.Fatal("Must have 3 or more records.")
	}

	host_cmd := ""

	// first argument is the name of the local command
	local_command := arg_list[0]

	// select the key to connect to the right server
	if strings.Contains(local_command, "mcpi") {
		host_cmd = "mcpi"
	}
	if strings.Contains(local_command, "ftb") {
		host_cmd = "ftb"
	}

	// get private key
	privateKeyPath := "/home/ktoks/.ssh/id_ed25519"
	key, err := os.ReadFile(privateKeyPath)
	if err != nil {
		log.Fatalf("Unable to open private key: %v", err)
	}

	// parse private key
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatalf("Unable to parse private key: %v", err)
	}

	// get ssh config
	config := &ssh.ClientConfig{
		User: "ktoks",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	addr := host_cmd + ":22"

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		log.Fatalf("Unable to connect: %v", err)
	}
	defer client.Close()

	// all remote arguments including the remote command
	remote_cmd_str := strings.Join(arg_list[1:], " ")

	session, err := client.NewSession()
	if err != nil {
		log.Fatalf("Unable to create session: %v", err)
	}
	defer session.Close()

	output, err := session.CombinedOutput(remote_cmd_str)
	if err != nil {
		log.Fatalf("Command failed: %v", err)
	}

	fmt.Printf("\n%s\n", output)
}
