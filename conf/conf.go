package conf

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/perfect-panel/ppanel-node/common/json5"
)

type Conf struct {
	LogConfig   LogConfig       `json:"Log"`
	CoresConfig []CoreConfig    `json:"Cores"`
	NodeConfig  []NodeConfig    `json:"Nodes"`
	ApiConfig   ServerApiConfig `json:"Api"`
}

type ServerApiConfig struct {
	ApiHost   string `json:"ApiHost"`
	ServerId  int    `json:"ServerID"`
	SecretKey string `json:"SecretKey"`
	Timeout   int    `json:"Timeout"`
}

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level:  "info",
			Output: "",
		},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	defer f.Close()

	reader := json5.NewTrimNodeReader(f)
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}

	err = json.Unmarshal(data, p)
	if err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}

	return nil
}
