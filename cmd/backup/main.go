package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/swat-engineering/borg-backup-remotely/internal/config"
	specialSSH "github.com/swat-engineering/borg-backup-remotely/internal/ssh"
)

func readConfig() *config.BorgBackups {
	log.Info("Reading config from stdin")
	var cfg config.BorgBackups
	if err := toml.NewDecoder(os.Stdin).DisallowUnknownFields().Decode(&cfg); err != nil {
		var details *toml.DecodeError
		if errors.As(err, &details) {
			fmt.Println(details.String())
		}
		var strictError *toml.StrictMissingError
		if errors.As(err, &strictError) {
			fmt.Println(strictError.String())
		}
		log.WithError(err).Fatal("Error parsing config file")
	}
	return &cfg
}

func main() {
	cfg := readConfig()
	log.WithField("Servers", len(cfg.Servers)).Info("Loaded configuration")
	finished := make(chan done)

	log.Info("Scheduling backups on the servers in parallel")
	for _, server := range cfg.Servers {
		go backupServer(cfg.Borg, server, finished)
	}

	anyError := false
	completed := 0
	for completed < len(cfg.Servers) {
		d := <-finished
		completed += 1
		if d.Err != nil {
			anyError = true
			log.WithField("name", d.Name).WithError(d.Err).Error("failed backup")
		} else {
			log.WithField("name", d.Name).Info("backup finished running")
		}
		if d.Output != nil {
			if _, err := os.Stdout.WriteString("Output of " + d.Name + ":\n"); err != nil {
				log.WithError(err).Error("Could not write output to Stdout")
			}
			if _, err := io.Copy(os.Stdout, d.Output); err != nil {
				log.WithError(err).Error("Could not copy output of backup process to our stdout")
			}
		}
	}
	if anyError {
		os.Exit(1)
	}
}

func getBackupRawKey(bc config.BorgConfig) *interface{} {
	return specialSSH.ParseSshRawKey(bc.BackupSshKey)
}

func getPruneRawKey(bc config.BorgConfig) *interface{} {
	return specialSSH.ParseSshRawKey(bc.PruneSshKey)
}

type borg struct {
	keyring    agent.Agent
	con        *ssh.Client
	output     io.Writer
	mainConfig config.BorgConfig
	boxTarget  config.BorgRepo
}

func setupBorgKnownHost(cfg config.BorgConfig, con *ssh.Client, output io.Writer) error {
	ses, err := specialSSH.NewSession(con, output)
	if err != nil {
		return fmt.Errorf("building new session: %w", err)
	}
	defer ses.Close()
	if err := ses.Run("grep -qxF '" + cfg.Server.KnownHost + "' ~/.ssh/known_hosts || echo \"" + cfg.Server.KnownHost + "\" >> ~/.ssh/known_hosts"); err != nil {
		return fmt.Errorf("setting known hosts: %w", err)
	}
	return nil
}

func buildBorg(box config.BorgRepo, cfg config.BorgConfig, con *ssh.Client, sshAgent agent.Agent, output io.Writer) (*borg, error) {
	// let's make sure the known hosts is fixed for the borg server
	if err := setupBorgKnownHost(cfg, con, output); err != nil {
		return nil, err
	}

	return &borg{
		con:        con,
		keyring:    sshAgent,
		output:     output,
		mainConfig: cfg,
		boxTarget:  box,
	}, nil
}

func (b *borg) pipeSingleConnection(target string) (string, error) {
	socName := fmt.Sprintf("/tmp/backup-connection-%d.sock", rand.Uint64())
	sshString := fmt.Sprintf("-o ProxyCommand='socat - UNIX-CLIENT:%s'", socName)
	con, err := b.con.ListenUnix(socName)
	if err != nil {
		return "", fmt.Errorf("could not open a unix socket listener: %w", err)
	}
	go func() {
		log.WithField("con", con).Info("waiting for connection to unix socket")
		newConnection, err := con.Accept()
		log.WithField("con", newConnection).Info("Got open for the unix socket")
		if err != nil {
			log.WithError(err).Error("Never got an open to this unix socket")
		}
		defer newConnection.Close()
		remote, err := net.Dial("tcp", target)
		if err != nil {
			log.WithError(err).WithField("target", target).Error("could not open target for proxying")
			return
		}
		log.WithField("remote", remote).WithField("target", target).Info("Established")
		defer remote.Close()
		specialSSH.Proxy(newConnection, remote)
	}()

	return sshString, nil
}

func (b *borg) exec(cmd string) error {
	ses, err := specialSSH.NewSession(b.con, b.output)
	if err != nil {
		return fmt.Errorf("building new session: %w", err)
	}
	defer ses.Close()

	borgRepo := fmt.Sprintf("ssh://%s@%s/%s/%s",
		b.mainConfig.Server.Username,
		b.mainConfig.Server.Host,
		b.mainConfig.RootDir,
		b.boxTarget.SubDir,
	)
	if err := ses.Setenv("BORG_REPO", borgRepo); err != nil {
		return fmt.Errorf("couldn't set remote BORG_REPO: %w", err)
	}

	forwardedBorgRepo, err := b.pipeSingleConnection(b.mainConfig.Server.Host)
	if err != nil {
		return fmt.Errorf("could not start forwarding connection for server: %w", err)
	}

	if err := ses.Setenv("BORG_RSH", fmt.Sprintf("ssh %s -oBatchMode=yes", forwardedBorgRepo)); err != nil {
		return fmt.Errorf("couldn't set remote BORG_RSH: %w", err)
	}

	if err = b.keyring.RemoveAll(); err != nil {
		log.WithError(err).Error("Could not clear keyring before adding a new key")
	}
	// we load the key for backup very shortly in the KeyRing, so that there is a very short window to catch it.
	err = b.keyring.Add(agent.AddedKey{
		PrivateKey:   *getBackupRawKey(b.mainConfig),
		LifetimeSecs: 2,
	})
	if err != nil {
		return fmt.Errorf("cannot add key: %w", err)
	}
	log.WithField("cmd", cmd).Debug("running borg command")
	//ses.Stdin = strings.NewReader(stdin)
	inPipe, err := ses.StdinPipe()
	if err != nil {
		return fmt.Errorf("in pipe: %w", err)
	}
	defer inPipe.Close()
	if _, err = b.output.Write([]byte("remote> " + cmd + "\n")); err != nil {
		log.WithError(err).Error("Unexpected failure to write to output")
	}
	if err := ses.Start(cmd); err != nil {
		return fmt.Errorf("starting command: %w", err)
	}
	time.Sleep(3 * time.Second) // give the password prompt time to show up
	// we write the passphrase to the std, to avoid keeping it in environment
	if _, err = inPipe.Write([]byte(b.boxTarget.Passphrase + "\r\n")); err != nil {
		log.WithError(err).Info("Could not write the passphrase to borg, which might be valid for init of repo")
	}
	return ses.Wait()

}

func buildSingleHostAgentSock(keyring agent.Agent) (string, error) {
	sockAgent := fmt.Sprintf("/tmp/agent-%p.sock", &keyring)
	if err := os.RemoveAll(sockAgent); err != nil {
		return "", fmt.Errorf("cleaning old socket file: %w", err)
	}
	sockAgentListener, err := net.Listen("unix", sockAgent)
	if err != nil {
		return "", fmt.Errorf("creating socket file: %w", err)
	}
	go func() {
		defer sockAgentListener.Close()
		defer os.RemoveAll(sockAgent)

		// only allow a single connection to the agent
		con, err := sockAgentListener.Accept()
		if err != nil {
			log.WithError(err).Error("accepting new agent socket")
			return
		}
		defer con.Close()
		if err = agent.ServeAgent(keyring, con); err != nil && err != io.EOF {
			log.WithError(err).Error("running agent on unix socket")
		}
	}()
	return sockAgent, nil
}

// run a borg command locally so that the secrets
// aren't shared to the server being back-upped
func (b *borg) execLocal(cmd string) error {
	splitCommand := strings.Split(cmd, " ")
	borgCommand := exec.Command(splitCommand[0], splitCommand[1:]...) // #nosec G204
	borgCommand.Env = append(borgCommand.Env, fmt.Sprintf("BORG_REPO=ssh://%s@%s/%s/%s", b.mainConfig.Server.Username, b.mainConfig.Server.Host, b.mainConfig.RootDir, b.boxTarget.SubDir))
	borgCommand.Env = append(borgCommand.Env, "BORG_PASSPHRASE="+b.boxTarget.Passphrase)

	knownHostFile, err := specialSSH.CreateKnownHostFile(b.mainConfig.Server.KnownHost)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(knownHostFile)
	borgCommand.Env = append(borgCommand.Env, "BORG_RSH=ssh -oBatchMode=yes -oUserKnownHostsFile="+knownHostFile)

	// we create a local ssh auth socket that can only be connected to once
	// to reduce the window of other processes on the system hijacking the auth agent
	sockAgent, err := buildSingleHostAgentSock(b.keyring)
	if err != nil {
		return err
	}
	borgCommand.Env = append(borgCommand.Env, "SSH_AUTH_SOCK="+sockAgent)

	if err := specialSSH.PipeClosableStreams(borgCommand, b.output); err != nil {
		return err
	}

	if err = b.keyring.RemoveAll(); err != nil {
		return fmt.Errorf("could not clear keyring: %w", err)
	}
	if err = b.keyring.Add(agent.AddedKey{
		PrivateKey:   *getPruneRawKey(b.mainConfig),
		LifetimeSecs: 2,
	}); err != nil {
		return fmt.Errorf("failed loading key in keyring: %w", err)
	}

	if _, err := b.output.Write([]byte("local> " + cmd + "\n")); err != nil {
		log.WithError(err).Error("Could not write to output buffer")
	}

	return borgCommand.Run()
}

// I got this far!

type done struct {
	Name   string
	Err    error
	Output io.ReadWriter
}

type ThreadSafeBuffer struct {
	b bytes.Buffer
	m sync.Mutex
}

func (b *ThreadSafeBuffer) Read(p []byte) (n int, err error) {
	b.m.Lock()
	defer b.m.Unlock()
	return b.b.Read(p)
}
func (b *ThreadSafeBuffer) Write(p []byte) (n int, err error) {
	b.m.Lock()
	defer b.m.Unlock()
	return b.b.Write(p)
}
func (b *ThreadSafeBuffer) String() string {
	b.m.Lock()
	defer b.m.Unlock()
	return b.b.String()
}

func backupServer(backupConfig config.BorgConfig, server config.Server, finished chan done) {
	result := done{
		Name:   server.Name,
		Output: new(ThreadSafeBuffer),
	}
	keyRing := agent.NewKeyring()
	mainCon, proxyCon, err := specialSSH.OpenSSHConnection(server.Connection, keyRing)
	if err != nil {
		result.Err = err
		finished <- result
		return
	}

	defer mainCon.Close()
	if proxyCon != nil {
		defer proxyCon.Close()
	}

	borg, err := buildBorg(server.BorgTarget, backupConfig, mainCon, keyRing, result.Output)
	if err != nil {
		result.Err = err
		finished <- result
		return
	}

	err = borg.exec("borg init --encryption=repokey-blake2")
	if err != nil {
		reInit := false
		var initError *ssh.ExitError
		if errors.As(err, &initError) {
			reInit = initError.ExitStatus() == 2
		}
		if !reInit {
			result.Err = fmt.Errorf("creating borg repo: %w", err)
			finished <- result
			return
		} else {
			log.Debug("The borg repo was already initialized, which is excepted for daily backups")
		}
	}

	cmd := "borg create --lock-wait 600 --files-cache=ctime,size --noctime --noflags --noflags --stats"
	for _, e := range server.Excludes {
		cmd = cmd + " --exclude '" + e + "'"
	}
	cmd = cmd + " ::snapshot-{utcnow}"
	for _, p := range server.SourcePaths {
		cmd = cmd + " '" + p + "'"
	}

	err = borg.exec(cmd)

	if err != nil {
		result.Err = fmt.Errorf("creating borg archive failed: %w", err)
		finished <- result
		return
	}

	err = borg.execLocal("borg prune --lock-wait 600 --stats --keep-daily 7 --keep-weekly 20 --keep-monthly 12 --keep-yearly 15")

	if err != nil {
		result.Err = fmt.Errorf("pruning borg archive failed: %w", err)
		finished <- result
		return
	}
	finished <- result
}
