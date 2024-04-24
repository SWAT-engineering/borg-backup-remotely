package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/pelletier/go-toml/v2"
	log "github.com/sirupsen/logrus"

	"github.com/swat-engineering/borg-backup-remotely/internal/borg"
	"github.com/swat-engineering/borg-backup-remotely/internal/config"
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
	reportError := func(err error) {
		result.Err = err
		finished <- result
	}

	borg, err := borg.BuildBorg(server, backupConfig, result.Output)
	if err != nil {
		reportError(err)
		return
	}
	defer borg.Close()

	err = borg.ExecLocal("borg init --encryption=repokey-blake2")
	if err != nil {
		reInit := false
		var initError *exec.ExitError
		log.Infof("%T", err)
		if errors.As(err, &initError) {
			reInit = initError.ExitCode() == 2
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

	err = borg.ExecRemote(cmd)

	if err != nil {
		result.Err = fmt.Errorf("creating borg archive failed: %w", err)
		finished <- result
		return
	}

	err = borg.ExecLocal("borg check --verbose")

	if err != nil {
		result.Err = fmt.Errorf("checking borg archive failed: %w", err)
		finished <- result
		return
	}

	err = borg.ExecLocal(fmt.Sprintf("borg prune --lock-wait 600 --stats %s", backupConfig.PruneSetting))

	if err != nil {
		result.Err = fmt.Errorf("pruning borg archive failed: %w", err)
		finished <- result
		return
	}
	finished <- result
}
