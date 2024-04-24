package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/pelletier/go-toml/v2"
	log "github.com/sirupsen/logrus"

	"github.com/swat-engineering/borg-backup-remotely/internal/borg"
	"github.com/swat-engineering/borg-backup-remotely/internal/config"
	"github.com/swat-engineering/borg-backup-remotely/internal/util"
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

func backupServer(backupConfig config.BorgConfig, server config.Server, finished chan done) {
	result := done{
		Name:   server.Name,
		Output: new(util.ThreadSafeBuffer),
	}
	result.Err = runBackupServer(backupConfig, server, result.Output)
	finished <- result

}

func runBackupServer(backupConfig config.BorgConfig, server config.Server, output io.ReadWriter) error {
	borg, err := borg.BuildBorg(server, backupConfig, output)
	if err != nil {
		return err
	}
	defer borg.Close()

	myLog := log.WithField("name", server.Name)

	myLog.Info("Connections established")

	myLog.Info("Initializing borg repo if needed")
	err = borg.ExecLocal("borg init --encryption=repokey-blake2")
	if err != nil {
		reInit := false
		var initError *exec.ExitError
		if errors.As(err, &initError) {
			reInit = initError.ExitCode() == 2
		}
		if !reInit {
			return fmt.Errorf("creating borg repo: %w", err)
		} else {
			myLog.Debug("The borg repo was already initialized, which is excepted for daily backups")
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

	myLog.Info("Creating new borg archive")
	err = borg.ExecRemote(cmd)
	if err != nil {
		return fmt.Errorf("creating borg archive failed: %w", err)
	}

	myLog.Info("Pruning old backups")
	err = borg.ExecRemote(fmt.Sprintf("borg prune --lock-wait 600 --stats %s", backupConfig.PruneSetting))
	if err != nil {
		return fmt.Errorf("pruning borg archive failed: %w", err)
	}

	myLog.Info("Checking if client was maybe messing things up for us")
	err = borg.ExecLocal("borg check")
	if err != nil {
		myLog.WithError(err).Fatal("*** Borg check failed, this tends to indicate someone is messing with the data ***")
		return fmt.Errorf("borg check failed, data corruption? : %w", err)
	}

	myLog.Info("Applying deletes if the checks were successful")
	err = borg.ExecLocal("borg compact")
	if err != nil {
		return fmt.Errorf("borg compact failed, data corruption? : %w", err)
	}
	return nil
}
