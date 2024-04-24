# Borg Backup Remotely

A tool to trigger multiple remote [borg backups](https://www.borgbackup.org/), being careful about who has access to which ssh key and other secrets.

Before you ask, is it overkill? Maybe, but this is as close to zero trust as I could get it. It's an improvement of [borg-backups pull documentation](https://borgbackup.readthedocs.io/en/stable/deployment/pull-backup.html).

In short summary:

- minimal trust in the servers you want to backup
- minimal trust in the server where the backup is stored (this is [the basic security model of borg backup](https://borgbackup.readthedocs.io/en/stable/internals/security.html))
- trust the server that initiates the backups, it has all the keys, and orchestrates the backup. Ideally it's a on-demand constructed vm/container

How? Well, given a borg backup server B, and a machine where this program is running C, and a server to be backed-up T:

- C sets up an single ssh connection to B
- C sets up an single ssh connection to T
- C creates a single-use unix socket at T and forwards it to a `--append-only` & location constrained `borg serve` session on B
- C invokes `borg` commands on T that use the unix socket for the connection to B
- C sends the passphrase to T over stdin pipe instead of environment flags
- C invokes `borg compact` locally (against a local unix socket that connects to a `borg serve` session on B)

In essence, we use multiple SSH sessions in a single SSH connection and forward everything connections over unix-sockets instead of un-regulated local TCP ports that any process can connect to. C never forwards keys/agents to B or T.

This way:

- T does not need to have network access to B (and vice versa)
- T never has any ssh-key local that can connect to the B server (and vice versa)
- T never has the passphrase of the borg backup repo in the environment or a local file
- If someone were to take over T steal the unix socket, they would only be do `append-only` operations
- You have to make sure C is secure, as it knows all the private keys and can connect to B

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
RootDir="/home/backups" # this is the main folder on your backup server where everything gets rooted under, has to be absolute
PruneSetting="--keep-daily 7 --keep-weekly 20 --keep-monthly 12 --keep-yearly 15"

[Borg.Server]
Host= "target-borg-host:<port>" # it port is not supplied, port 22 is assumed
UserName= "borg-user-name-for-backups"
KnownHost = """
...
""" # result of `ssh-keyscan <target-borg-host>`
PrivateKey = """
-----BEGIN OPENSSH PRIVATE KEY-----
...
-----END OPENSSH PRIVATE KEY-----
""" # key of the "borg-user-name-for-backups", no `command` specification in the authorized_keys


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

- ssh key that allows us to run borg
- make sure is reachable by the server that coordinates the backups


### per server to backup

- make sure borg backup & socat are installed
- ssh key of the user that has read rights of the directories you want to backup.
- adapt these settings in `/etc/ssh/sshd_config`:

```sshdconf
AllowStreamLocalForwarding yes
AllowTcpForwarding yes
StreamLocalBindUnlink yes
AcceptEnv BORG_*
```

## server that runs this command

- install borg and socat
- make sure servers to backup are reachable
- make sure backup server is reachable