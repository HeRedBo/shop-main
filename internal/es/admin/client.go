// Package admin 是 Elasticsearch 管理适配层的基础设施部分，
// 统一提供对共享库 github.com/HeRedBo/pkg/es 客户端的安全访问入口。
// 索引 CRUD / 别名 / bulk 等管理能力由后续阶段在同目录其他文件中补齐。
package admin

import (
	"errors"
	"fmt"

	"shop/pkg/global"

	"github.com/HeRedBo/pkg/es"
	"github.com/olivere/elastic/v7"
)

// initHint 客户端未初始化时的统一指引信息
const initHint = "请检查 conf/config.yml 的 elasticsearch.enabled 是否为 true，" +
	"以及命令入口是否通过 bootstrap.WithElasticsearch() 启用了 ES 初始化"

// Client 返回全局 ES 客户端；未初始化时返回带指引的 error，避免调用方 nil 解引用 panic
func Client() (*es.Client, error) {
	if global.EsClient == nil {
		return nil, fmt.Errorf("Elasticsearch 客户端未初始化：%s", initHint)
	}
	return global.EsClient, nil
}

// Raw 返回底层原生 *elastic.Client（olivere/elastic v7 逃生舱），
// 用于 pkg/es 未封装的索引/别名/bulk 等操作；调用方必须持有 ctx。
func Raw() (*elastic.Client, error) {
	c, err := Client()
	if err != nil {
		return nil, err
	}
	// 防御性校验：理论上 newClient 成功后该字段必然非 nil
	if c.Client == nil {
		return nil, errors.New("Elasticsearch 底层 elastic.Client 未初始化（InitClientWithOptions 可能静默失败），" +
			"请检查 es 模块日志中的 init bulkProcessor error / connection error 记录")
	}
	return c.Client, nil
}

// Close 关闭全局 ES 客户端（含 BulkProcessor）。
// 常规生命周期由 bootstrap.Shutdown 统一管理，此函数仅供特殊场景（如测试）显式释放。
// 注意：共享库 es.CloseAll() 对 BulkProcessor 无 nil 判断，调用方需确保客户端已完成初始化。
func Close() error {
	if global.EsClient == nil {
		// 未初始化时直接跳过，避免 CloseAll 内部 nil 解引用 panic
		return nil
	}
	es.CloseAll()
	return nil
}
