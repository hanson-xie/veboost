package config

import (
	"fmt"
	"sync"

	"github.com/spf13/viper"
)

var (
	instance *AppConfig
	once     sync.Once
)

// AppConfig represents the entire application configuration
type AppConfig struct {
	Logger  LogConfig         `mapstructure:"logger"`
	DocAuth map[string]string `mapstructure:"doc_auth"`
	DNS     struct {
		Endpoint string `mapstructure:"endpoint"`
	} `mapstructure:"dns"`
	DBServers struct {
		VeDsnPsql  string `mapstructure:"ve_dsn_psql"`
		VeDsnMysql string `mapstructure:"ve_dsn_mysql"`
	} `mapstructure:"db_servers"`
	Contracts struct {
		Airdrop struct {
			RPC     string `mapstructure:"rpc"`
			Address string `mapstructure:"address"`
		} `mapstructure:"airdrop"`
	} `mapstructure:"contracts"`
	Airdrop struct {
		ExpireEpochs uint64 `mapstructure:"expire_epochs"`
	} `mapstructure:"airdrop"`
}

// LogConfig represents logger configuration
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Stdout bool   `mapstructure:"stdout"`
	File   struct {
		Path       string `mapstructure:"path"`
		MaxSize    int    `mapstructure:"max_size"`
		MaxBackups int    `mapstructure:"max_backups"`
		MaxAge     int    `mapstructure:"max_age"`
		Compress   bool   `mapstructure:"compress"`
	} `mapstructure:"file"`
}

// InitConfig initializes the configuration singleton
func InitConfig(configPath string) error {
	var err error
	once.Do(func() {
		instance = &AppConfig{}
		viper.SetConfigFile(configPath)
		if err = viper.ReadInConfig(); err != nil {
			err = fmt.Errorf("failed to read config file: %v", err)
			return
		}

		if err = viper.Unmarshal(instance); err != nil {
			err = fmt.Errorf("failed to unmarshal config: %v", err)
			return
		}
	})
	return err
}

// GetConfig returns the configuration singleton instance
func GetConfig() *AppConfig {
	if instance == nil {
		panic("configuration not initialized")
	}
	return instance
}
