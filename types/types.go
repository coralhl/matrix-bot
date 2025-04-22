package types

type Config struct {
	Homeserver string `yaml:"homeserver"`
	Botname    string `yaml:"botname"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	LogLevel   string `yaml:"loglevel"`
	DB         DB     `yaml:"database"`
}

type DB struct {
	DBPath     string `yaml:"dbpath"`
	Pickle     string `yaml:"pickle"`
}
