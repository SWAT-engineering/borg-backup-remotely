package ssh

import (
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/swat-engineering/borg-backup-remotely/internal/config"
)

func parseSshKey(privateKey string) ssh.Signer {
	key, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		log.WithError(err).Fatal("parsing key failed")
	}
	return key
}

func createKnownHostFile(hosts ...string) (tempKnownHostFile string, err error) {
	knownHostTemp, err := os.CreateTemp("", "ssh-known-*")
	if err != nil {
		return "/dev/null", fmt.Errorf("opening SSH key: %w", err)
	}

	err = os.WriteFile(knownHostTemp.Name(), []byte(strings.Join(hosts, "\n")), os.FileMode(0700))
	if err != nil {
		return "/dev/null", fmt.Errorf("writing custom known hosts: %w", err)
	}
	return knownHostTemp.Name(), nil
}

func createKnownHostChecker(hosts ...string) (ssh.HostKeyCallback, error) {
	tempFile, err := createKnownHostFile(hosts...)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tempFile)

	return knownhosts.New(tempFile)
}

func dialSsh(con config.Connection) (*ssh.Client, *ssh.Client, error) {
	khCallback, err := createKnownHostChecker(con.KnownHost, con.ProxyJumpKnownHost)
	if err != nil {
		return nil, nil, fmt.Errorf("constructing knownhost validator: %w", err)
	}

	config := &ssh.ClientConfig{
		User: con.Username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(parseSshKey(con.PrivateKey)),
		},
		HostKeyCallback: khCallback,
	}
	log.WithField("config", config).Debug("Connecting to server")

	var client *ssh.Client
	var proxyJumpClient *ssh.Client
	targetAddr := con.Host
	if !strings.Contains(targetAddr, ":") {
		targetAddr += ":22"
	}
	if con.ProxyJumpHost != "" {
		targetJumpAddr := con.ProxyJumpHost
		if !strings.Contains(targetJumpAddr, ":") {
			targetJumpAddr += ":22"
		}
		log.WithField("jump", targetJumpAddr).Debug("Connecting to jump host first")
		proxyJumpClient, err = ssh.Dial("tcp", targetJumpAddr, config)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to proxyJumpHost: %w", err)
		}

		log.WithField("target", targetAddr).Debug("Connecting to target via jump")
		jumpedDial, err := proxyJumpClient.Dial("tcp", targetAddr)
		if err != nil {
			defer proxyJumpClient.Close()
			return nil, nil, fmt.Errorf("connecting to host via proxy: %w", err)
		}
		// now we use this tcp socket to create an ssh connection
		c, chans, regs, err := ssh.NewClientConn(jumpedDial, targetAddr, config)
		if err != nil {
			defer proxyJumpClient.Close()
			defer jumpedDial.Close()
			return nil, nil, fmt.Errorf("opening ssh connection to host via proxy: %w", err)
		}
		client = ssh.NewClient(c, chans, regs)
	} else {
		log.WithField("target", targetAddr).Debug("Connecting to target")
		client, err = ssh.Dial("tcp", targetAddr, config)
		if err != nil {
			return nil, nil, fmt.Errorf("opening ssh connection:  %w", err)
		}
	}

	return client, proxyJumpClient, nil
}

func sendKeepAlive(client *ssh.Client, every time.Duration, maxErrors int) chan<- bool {
	done := make(chan bool, 1)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()

		fails := 0
		for {
			select {
			case <-t.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					fails++
					if fails >= maxErrors {
						log.WithError(err).Debug("Stopping ssh client due to keepalive misses")
						if err := client.Close(); err != nil {
							log.WithError(err).Error("Could not close ssh session")
						}
						return
					}
				} else {
					fails = 0
				}
			case <-done:
				log.Debug("Gracefully stop sending keep-alives")
				return
			}
		}
	}()
	return done
}
