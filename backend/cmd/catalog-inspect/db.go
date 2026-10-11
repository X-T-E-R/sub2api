package main

import (
	"context"
	"database/sql"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/repository"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// catalogPoolMaxOpenConns 是本工具自己的连接上限。巡检是严格串行的短流程，
// 不继承线上服务按并发调优的连接池，避免临时工具占用生产连接数。
const catalogPoolMaxOpenConns = 2

// openCatalogDB 只做 InitEnt 中“能读”的那一部分：
//
//	正式时区初始化 → 正式 DSN → Postgres 方言驱动 → ent.NewClient → repository.ProvideSQLDB。
//
// 刻意不做 InitEnt 的另外三步：执行迁移（applyMigrationsFS）、补齐启动密钥
// （ensureBootstrapSecrets）、SIMPLE 模式补默认分组。它们是启动期写入，超出本工具
// “只允许令牌刷新写”的边界。
//
// 三个复用点都是正式代码：cfg.Database.DSNWithTimezone、internal/pkg/timezone.Init、
// repository.ProvideSQLDB；驱动构造与 InitEnt 完全一致（entsql.Open + ent.NewClient）。
// 未复用的只有内部函数 applyDBPoolSettings / newServerTimingConnector（未导出，且与只读取数无关）。
//
// 失败一律返回有限 stage 错误：DSN 与驱动错误可能内嵌 `password=`，因此不向上传递原始文本。
// 返回的 *dbent.Client 由调用方 Close（会一并关闭底层 *sql.DB）。
func openCatalogDB(ctx context.Context, cfg *config.Config) (*dbent.Client, *sql.DB, error) {
	if cfg == nil {
		return nil, nil, newStageError(stageDatabaseOpenFailed)
	}
	if err := timezone.Init(cfg.Timezone); err != nil {
		return nil, nil, newStageError(stageTimezoneInitFailed)
	}

	driver, err := entsql.Open(dialect.Postgres, cfg.Database.DSNWithTimezone(cfg.Timezone))
	if err != nil {
		return nil, nil, newStageError(stageDatabaseOpenFailed)
	}

	client := dbent.NewClient(dbent.Driver(driver))
	db, err := repository.ProvideSQLDB(client)
	if err != nil {
		_ = client.Close()
		return nil, nil, newStageError(stageDatabaseOpenFailed)
	}
	db.SetMaxOpenConns(catalogPoolMaxOpenConns)

	// 先做一次可达性探测，让连不上数据库时给出全局 stage，而不是三个账号各自报错。
	if err := db.PingContext(ctx); err != nil {
		_ = client.Close()
		return nil, nil, newStageError(stageDatabaseUnreachable)
	}
	return client, db, nil
}
