package main

import (
	"github.com/HeRedBo/pkg/db"
	"github.com/HeRedBo/pkg/mq"
	"github.com/gin-gonic/gin"
	"shop/pkg/base"
	"shop/pkg/casbin"
	"shop/pkg/global"
	"shop/pkg/jwt"
	"shop/pkg/logging"
)

func init() {
	global.LoadConfig()
	global.LOG = base.SetupLogger()
	logging.Init()

	//初始化mysql
	err := db.InitMysqlClient(db.DefaultClient, global.CONFIG.Database.User,
		global.CONFIG.Database.Password, global.CONFIG.Database.Host,
		global.CONFIG.Database.Name)
	if err != nil {
		global.LOG.Error("InitMysqlClient error", err, "client", db.DefaultClient)
	}
	global.Db = db.GetMysqlClient(db.DefaultClient).DB
	casbin.InitCasbin(global.Db)
	jwt.Init()

	err = mq.InitSyncKafkaProducer(mq.DefaultKafkaSyncProducer, global.CONFIG.Kafka.Hosts, nil)
	if err != nil {
		global.LOG.Error("InitSyncKafkaProducer err", err, "client", mq.DefaultKafkaSyncProducer)
		panic(err)
	}
}

func main() {
	gin.SetMode(global.CONFIG.Server.RunMode)

}
