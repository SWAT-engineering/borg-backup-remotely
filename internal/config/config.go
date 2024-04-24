package config

type BorgBackups struct {
	Borg    BorgConfig
	Servers []Server
}

type BorgConfig struct {
	Server       Connection
	RootDir      string
	PruneSetting string
}

type Connection struct {
	Host               string
	KnownHost          string
	ProxyJumpHost      string
	ProxyJumpKnownHost string
	Username           string
	PrivateKey         string
}

type BorgRepo struct {
	SubDir     string
	Passphrase string
}

type Server struct {
	Name        string
	SourcePaths []string
	Excludes    []string

	Connection Connection
	BorgTarget BorgRepo
}
