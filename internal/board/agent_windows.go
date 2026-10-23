//go:build windows

package board

import (
	sshagent "github.com/xanzy/ssh-agent"
	"golang.org/x/crypto/ssh"
)

// agentSigners returns the keys held by the Windows OpenSSH agent (or Pageant), if one is running.
func agentSigners() []ssh.Signer {
	if !sshagent.Available() {
		return nil
	}
	a, _, err := sshagent.New()
	if err != nil {
		return nil
	}
	list, err := a.Signers()
	if err != nil {
		return nil
	}
	return list
}
