package ssh

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/swat-engineering/borg-backup-remotely/internal/config"
	"golang.org/x/crypto/ssh"
)

type SshConnection struct {
	client         *ssh.Client
	jumpClient     *ssh.Client
	keepAlives     chan<- bool
	keepAlivesJump chan<- bool
}

func SetupConnection(target config.Connection) (SshConnection, error) {
	con, jumpCon, err := dialSsh(target)
	if err != nil {
		return SshConnection{}, err
	}
	result := SshConnection{nil, nil, nil, nil}
	result.client = con
	result.keepAlives = sendKeepAlive(con, 10*time.Second, 30)
	if jumpCon != nil {
		result.jumpClient = jumpCon
		result.keepAlivesJump = sendKeepAlive(con, 10*time.Second, 30)
	}
	return result, nil
}

func (c SshConnection) ListenUnix(socketPath string) (net.Listener, error) {
	return c.client.ListenUnix(socketPath)
}

func (c SshConnection) ExecuteSingleCommand(cmd string, stdIn io.Reader, stdOut io.Writer, stdErr io.Writer, env map[string]string) error {
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("opening ssh session: %w", err)
	}
	log.WithField("session", session).Info("Opened session")
	defer session.Close()

	for k, v := range env {
		if err := session.Setenv(k, v); err != nil {
			return fmt.Errorf("could not set %s in session: %w", k, err)
		}
	}

	session.Stdin = stdIn
	session.Stdout = stdOut
	session.Stderr = stdErr

	return session.Run(cmd)
}

func (c *SshConnection) Close() error {
	if c.client != nil {
		defer func() {
			c.client = nil
			c.jumpClient = nil
		}()

		c.keepAlives <- false
		errClient := c.client.Close()

		if c.jumpClient != nil {
			c.keepAlivesJump <- false
			if err := c.jumpClient.Close(); err != nil {
				return err
			}
		}
		return errClient
	} else {
		return errors.New("already closed connection")
	}
}
