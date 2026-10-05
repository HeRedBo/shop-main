package global

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/silenceper/wechat/v2/officialaccount"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"shop/conf"
	"shop/pkg/logging"
)

var (
	Db             *gorm.DB
	LOG            *zap.SugaredLogger
	CONFIG         conf.Config
	WechatOfficial *officialaccount.OfficialAccount
)

// GetLogger 获取指定业务模块的 logger
func GetLogger(module string) *zap.SugaredLogger {
	return logging.GetLogger(module)
}

// 加载配置，失败直接panic
func LoadConfig() {
	LoadConfigWithPath("conf/config.yml")
}

// LoadConfigWithPath 从指定路径加载配置文件
func LoadConfigWithPath(configPath string) {
	v := viper.New()
	//1.设置配置文件路径
	v.SetConfigFile(configPath)
	//2.配置读取
	if err := v.ReadInConfig(); err != nil {
		panic(err)
	}
	//3.将配置映射成结构体
	if err := v.Unmarshal(&CONFIG); err != nil {
		panic(err)
	}

	//4. 监听配置文件变动, 重新解析配置
	v.WatchConfig()
	v.OnConfigChange(func(e fsnotify.Event) {
		fmt.Println(e.Name)
		if err := v.Unmarshal(&CONFIG); err != nil {
			panic(err)
		}
	})
}
