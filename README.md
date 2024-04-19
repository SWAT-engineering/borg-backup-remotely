# Borg Backup Remotely

A tool to trigger multiple remote borg backups, being careful about who has access to which ssh key and other secrets. 

Before you ask, is it overkill? Maybe, but this is as close to zero secret as I could get it.

The process work as follows, given a borg backup server B, and a machine where this program is running C, and a server to be backed-up T.

- C setups ssh connection to T with and SSH agent forwarded
- C load append-only backup SSH key in SSH agent with a timeout of 3 seconds
- C sends borg create (that target B) to T sending the backup passphrase to the stdin
- T uses agent to create SSH connection, and then starts backup process
- C now sets up a tcp forwarding to B via T
- C creates a local SSH agent with the prune key loaded, but only allows for a single connection to it
- C now runs the borg prune command locally against B, but uses T for tcp forwarding

This way:

- T never has any ssh-key local that can connect to the B server
- T never has the passphrase of the borg backup repo in the environment or a local file
- If someone were to take over T, they could only get hold of the backup ssh-key, which only allows appends to the backup
- You have to make sure C is secure, as it knows all the private keys
- C doesn't have to have access to the B server, it only needs to connect to the servers that need to be backed-up

## Setup

### Install

- Either build from source (`backup` will be installed in `$GOPATH/bin`):

```
go install github.com/swat-engineering/borg-backup-remotely/cmd/backup@v0.1.0
```

- [or download pre-build binary](https://github.com/SWAT-engineering/borg-backup-remotely/releases/tag/v0.0.1-test1)
- or use pre-build docker image: `ghcr.io/SWAT-engineering/borg-backup-remotely:v0.1.0`

### Config

This tool takes an .toml file piped into the stdin. As this file contains the SSH private keys, you should not store this plain text.
Either use something like age and decrypt it to stdout, or use a different way to manage secrets.

Here is an example toml file:

```toml
## First we setup the borg connection info
[Borg]
RootDir="~/backups" # this is the main folder on your backup server where everything gets rooted under
BackupSshKey = """
-----BEGIN OPENSSH PRIVATE KEY-----
.....
-----END OPENSSH PRIVATE KEY-----
""" # the ssh key that is constrained to only append-mode, this is used by all the servers to send their backups
PruneSshKey = """
-----BEGIN OPENSSH PRIVATE KEY-----
.....
-----END OPENSSH PRIVATE KEY-----
""" # the ssh key that is not constrained to append-only mode
PruneSetting="--keep-daily 7 --keep-weekly 20 --keep-monthly 12 --keep-yearly 15"

[Borg.Server]
Host= "target-borg-host:<port>" # it port is not supplied, port 22 is assumed
UserName= "borg-user-name-for-backups"
KnownHost = """
...
""" # result of `ssh-keyscan <target-borg-host>`


# Then we setup the servers we want to backup
# per server we have a new [[Servers]] block
[[Servers]]
Name = "Display name of the server in the log"
SourcePaths=[
    "/one/or/more/paths/to/backup"
]
Excludes = [
    "/optional/exclude/globs/**/*.to-ignore"
]

# we then configure the borg target repo for this server
[Servers.BorgTarget]
SubDir="sub/dir/on/backup/machine"
Passphrase="pass-phrase-for-this-backup"

# and finally how to connect to the server
[Servers.Connection]
Username="user-with-read-rights"
Host="server-to-be-backed-up"
KnownHost="""
""" # result of `ssh-keyscan <server-to-be-backed-up>`
PrivateKey = """
-----BEGIN OPENSSH PRIVATE KEY-----
...
-----END OPENSSH PRIVATE KEY-----
""" # key of the <user-with-read-rights>
```

Note that the ssh-keys for prune & backup should be different. The key for the users with backup can be shared (even be the same as for example the backup key), this depends on your policies.

### setup on server with the borg archive

- borg SSH key constrained to a dir in append only mode. note that we'll create a borg backup repo per server as a subdir inside of this dir
- separate borg prune ssh key that is allowed to manipulate borg repos (prune them)
- make sure is reachable by the servers to be backed-up


### per server to backup
- make sure borg backup is installed
- ssh key of the user that has read rights of the directories you want to backup.
- allow tcp forwarding


## server that runs this script

- install borg
- make sure servers to backup are reachable