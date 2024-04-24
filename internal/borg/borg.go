package borg

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/swat-engineering/borg-backup-remotely/internal/config"
	specialSSH "github.com/swat-engineering/borg-backup-remotely/internal/ssh"
)

type Borg struct {
	backup  *specialSSH.SshConnection
	target  *specialSSH.SshConnection
	cfg     config.Server
	output  io.Writer
	rootCfg config.BorgConfig
}

func (b *Borg) Close() {
	defer b.backup.Close()
	defer b.target.Close()
}

func BuildBorg(cfg config.Server, root config.BorgConfig, output io.Writer) (*Borg, error) {
	backupConnection, err := specialSSH.SetupConnection(root.Server)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the backup server: %w", err)
	}

	targetConnection, err := specialSSH.SetupConnection(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the target server: %w", err)
	}

	return &Borg{
		cfg:     cfg,
		backup:  &backupConnection,
		target:  &targetConnection,
		output:  output,
		rootCfg: root,
	}, nil
}

const SOC_NAME = "/tmp/backup-connection.sock"

func (b *Borg) ExecRemote(cmd string) error {

	sock, err := b.target.ListenUnix(SOC_NAME)
	if err != nil {
		return fmt.Errorf("could not open a unix socket listener, make sure 'StreamLocalBindUnlink yes' is set in sshd_config: %w", err)
	}

	go b.proxySocketToBorgServer(sock, true)

	env := map[string]string{
		"BORG_REPO":                        b.calculateRepoUrl(),
		"BORG_RSH":                         fmt.Sprintf("sh -c 'socat - UNIX-CLIENT:%s'", SOC_NAME),
		"BORG_PASSCOMMAND":                 "cat -", // take passphrase from stdin
		"BORG_RELOCATED_REPO_ACCESS_IS_OK": "yes",
	}

	stdInPassphrase := bytes.NewReader([]byte(b.cfg.BorgTarget.Passphrase + "\r\n"))
	return b.target.ExecuteSingleCommand(cmd, stdInPassphrase, b.output, b.output, env)
}

func (b Borg) proxySocketToBorgServer(sock net.Listener, appendOnly bool) {
	defer sock.Close()
	c, err := sock.Accept()
	if err != nil {
		log.WithError(err).Error("never got an open to the unix socket")
		return
	}
	defer c.Close()
	var appendAllowed string
	if appendOnly {
		appendAllowed = "--append-only"
	} else {
		appendAllowed = ""
	}
	serveCmd := fmt.Sprintf("borg serve %s --restrict-to-path %s/%s", appendAllowed, b.rootCfg.RootDir, b.cfg.BorgTarget.SubDir)
	if err := b.backup.ExecuteSingleCommand(serveCmd, c, c, b.output, map[string]string{}); err != nil {
		log.WithError(err).Error("borg serve failed")
		return
	}
}

func (b *Borg) ExecLocal(cmd string) error {
	borgSocket, err := b.generateLocalSocket()
	if err != nil {
		return err
	}

	splitCommand := strings.Split(cmd, " ")
	borgCommand := exec.Command(splitCommand[0], splitCommand[1:]...) // #nosec G204
	borgCommand.Env = []string{
		"BORG_REPO=" + b.calculateRepoUrl(),
		"BORG_PASSPHRASE=" + b.cfg.BorgTarget.Passphrase,
		fmt.Sprintf("BORG_RSH=sh -c 'socat - UNIX-CLIENT:%s'", borgSocket),
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
	}

	borgCommand.Stdout = b.output
	borgCommand.Stderr = b.output

	if _, err := b.output.Write([]byte("local> " + cmd + "\n")); err != nil {
		log.WithError(err).Error("Could not write to output buffer")
	}

	return borgCommand.Run()

}

func (b *Borg) generateLocalSocket() (string, error) {
	borgSocket := fmt.Sprintf("/tmp/backup-%p.sock", b)
	if err := os.RemoveAll(borgSocket); err != nil {
		return "", fmt.Errorf("cleaning old socket file: %w", err)
	}

	borgListener, err := net.Listen("unix", borgSocket)
	if err != nil {
		return "", fmt.Errorf("creating socket file: %w", err)
	}
	go b.proxySocketToBorgServer(borgListener, false)

	return borgSocket, nil
}

func (b *Borg) calculateRepoUrl() string {
	return fmt.Sprintf("ssh://borg-server/%s/%s",
		b.rootCfg.RootDir,
		b.cfg.BorgTarget.SubDir,
	)
}
