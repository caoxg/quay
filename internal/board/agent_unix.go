//go:build !windows

package board

import (
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// agentSigners returns the keys held by the ssh-agent at $SSH_AUTH_SOCK, if any.
func agentSigners() []ssh.Signer {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}
	list, err := agent.NewClient(conn).Signers()
	if err != nil {
		return nil
	}
	return list
}
