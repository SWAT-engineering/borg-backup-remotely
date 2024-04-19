package connections

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/swat-engineering/borg-backup-remotely/internal/config"
)

func ForwardSingleConnection(localSSH net.Listener, con *ssh.Client, address string) {
	local, err := localSSH.Accept()
	if err != nil {
		log.WithError(err).Error("local forwarded connection failed")
		return
	}
	defer local.Close()

	remote, err := con.Dial("tcp", address)
	if err != nil {
		log.WithError(err).Error("local forwarded connection failed (forward connection)")
		return
	}
	defer remote.Close()

	done := make(chan bool, 2)
	go func() {
		io.Copy(local, remote)
		done <- true
	}()
	go func() {
		io.Copy(remote, local)
		done <- true
	}()

	// now we wait until either side is done
	<-done
	// end of this function will execute the deferred closes
}

func NewSession(client *ssh.Client, output io.Writer) (*ssh.Session, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("opening ssh session:  %w", err)
	}
	log.WithField("session", session).Info("Opened session")

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	if err := session.RequestPty("xterm", 40, 80, modes); err != nil {
		defer session.Close()
		return nil, fmt.Errorf("requesting pty: %w", err)
	}

	if err := PipeStreams(session, output); err != nil {
		defer session.Close()
		return nil, err
	}

	return session, nil

}

func ParseSshKey(privateKey string) ssh.Signer {
	key, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		log.WithError(err).Fatal("parsing key failed")
	}
	return key
}

func ParseSshRawKey(privateKey string) *interface{} {
	key, err := ssh.ParseRawPrivateKey([]byte(privateKey))
	if err != nil {
		log.WithError(err).Fatal("loading private key")
	}
	return &key
}

func CreateKnownHostFile(hosts ...string) (tempKnownHostFile string, err error) {
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
	tempFile, err := CreateKnownHostFile(hosts...)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tempFile)

	return knownhosts.New(tempFile)
}

func DialSsh(con config.Connection) (*ssh.Client, *ssh.Client, error) {
	khCallback, err := createKnownHostChecker(con.KnownHost, con.ProxyJumpKnownHost)
	if err != nil {
		return nil, nil, fmt.Errorf("constructing knownhost validator: %w", err)
	}

	config := &ssh.ClientConfig{
		User: con.Username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(ParseSshKey(con.PrivateKey)),
		},
		HostKeyCallback: khCallback,
	}
	log.WithField("config", config).Info("Connecting to server")

	var client *ssh.Client
	var proxyJumpClient *ssh.Client
	targetAddr := con.Host
	if !strings.Contains(targetAddr, ":") {
		targetAddr = targetAddr + ":22"
	}
	if con.ProxyJumpHost != "" {
		proxyJumpClient, err = ssh.Dial("tcp", con.ProxyJumpHost+":22", config)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to proxyJumpHost: %w", err)
		}
		jumpedDial, err := proxyJumpClient.Dial("tcp", targetAddr)
		if err != nil {
			defer proxyJumpClient.Close()
			return nil, nil, fmt.Errorf("connecting to host via proxy: %w", err)
		}
		c, chans, regs, err := ssh.NewClientConn(jumpedDial, targetAddr, config)
		if err != nil {
			defer proxyJumpClient.Close()
			defer jumpedDial.Close()
			return nil, nil, fmt.Errorf("opening ssh connection to host via proxy: %w", err)
		}
		client = ssh.NewClient(c, chans, regs)
	} else {
		client, err = ssh.Dial("tcp", targetAddr, config)
		if err != nil {
			return nil, nil, fmt.Errorf("opening ssh connection:  %w", err)
		}
	}

	return client, proxyJumpClient, nil
}

func ConfigureAgentForwarding(client *ssh.Client, sshAgent agent.Agent) error {
	if err := agent.ForwardToAgent(client, sshAgent); err != nil {
		return fmt.Errorf("setting up agent: %w", err)
	}
	// we open up an initial session, just to configure the agent forwarding
	ses, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("opening session: %w", err)
	}
	defer ses.Close()
	if err := agent.RequestAgentForwarding(ses); err != nil {
		return fmt.Errorf("setting up agent forwarding failed: %w", err)
	}
	return nil
}

func OpenSSHConnection(con config.Connection, sshAgent agent.Agent) (*ssh.Client, *ssh.Client, error) {
	// first we connect to the ssh server
	client, proxyJumpClient, err := DialSsh(con)
	if err != nil {
		return nil, nil, err
	}
	log.WithField("client", client).Info("Connected to server")

	// before we do anything, we have to initiate the agent forwarding
	if err = ConfigureAgentForwarding(client, sshAgent); err != nil {
		defer client.Close()
		if proxyJumpClient != nil {
			defer proxyJumpClient.Close()
		}
		return nil, nil, err
	}
	log.WithField("client", client).Info("Agent forwarding setup")

	return client, proxyJumpClient, nil
}

type PipeAbleStreams interface {
	StdoutPipe() (io.Reader, error)
	StderrPipe() (io.Reader, error)
}

type PipeAbleCloseStreams interface {
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)
}

func PipeStreams(source PipeAbleStreams, target io.Writer) error {
	outPipe, err1 := source.StdoutPipe()
	errPipe, err2 := source.StderrPipe()
	return pipeStreamsActual(outPipe, errPipe, err1, err2, target)
}

func PipeClosableStreams(source PipeAbleCloseStreams, target io.Writer) error {
	outPipe, err1 := source.StdoutPipe()
	errPipe, err2 := source.StderrPipe()
	return pipeStreamsActual(outPipe, errPipe, err1, err2, target)
}
func pipeStreamsActual(outPipe io.Reader, errPipe io.Reader, outError error, errError error, target io.Writer) error {
	if outError != nil {
		return fmt.Errorf("opening out pipe: %w", outError)
	}
	if errError != nil {
		return fmt.Errorf("opening err pipe: %w", errError)
	}

	go io.Copy(target, outPipe)
	go io.Copy(target, errPipe)
	return nil
}
