package database

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"yingce/backend/internal/logging"

	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Config struct {
	Driver  string
	DSN     string
	DataDir string
}

func Open(config Config) (*gorm.DB, error) {
	driver := strings.ToLower(strings.TrimSpace(config.Driver))
	if driver == "" {
		driver = "sqlite"
	}
	switch driver {
	case "sqlite":
		dsn := strings.TrimSpace(config.DSN)
		if dsn == "" {
			if err := os.MkdirAll(config.DataDir, 0o755); err != nil {
				return nil, err
			}
			dsn = config.DataDir + "/open_ai_canvas.db?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL&_txlock=immediate"
		}
		dsn = normalizeSQLiteDSN(dsn)
		return gorm.Open(sqlite.New(sqlite.Config{
			DriverName: sqliteDriverName,
			DSN:        dsn,
		}), &gorm.Config{Logger: logging.GormLogger()})
	case "postgres", "postgresql":
		dsn := strings.TrimSpace(config.DSN)
		if dsn == "" {
			return nil, errors.New("PostgreSQL 模式必须配置 DATABASE_URL")
		}
		return gorm.Open(postgres.New(postgres.Config{
			DSN:          dsn,
			OptionOpenDB: []stdlib.OptionOpenDB{stdlib.OptionBeforeConnect(configurePostgresConnection)},
		}), &gorm.Config{Logger: logging.GormLogger()})
	default:
		return nil, fmt.Errorf("不支持的数据库驱动：%s", driver)
	}
}

// normalizeSQLiteDSN 将 mattn/go-sqlite3 风格的连接参数补充为 modernc.org/sqlite 识别的 _pragma 参数，
// 确保无 CGO 构建下连接池中的每条连接都生效 busy_timeout、WAL、synchronous 和 foreign_keys。
func normalizeSQLiteDSN(dsn string) string {
	if sqliteDriverName != "sqlite" {
		return dsn
	}
	base, rawQuery, hasQuery := strings.Cut(dsn, "?")
	if !hasQuery || rawQuery == "" {
		return dsn
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return dsn
	}
	existing := make(map[string]bool, len(query["_pragma"]))
	for _, pragma := range query["_pragma"] {
		name, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(pragma)), "(")
		name, _, _ = strings.Cut(name, "=")
		existing[strings.TrimSpace(name)] = true
	}
	type pragmaMapping struct {
		param  string
		pragma string
	}
	// busy_timeout 必须最先执行，避免并发建连执行 journal_mode(WAL) 时因默认 0ms 超时直接报 SQLITE_BUSY。
	var additions []string
	for _, item := range []pragmaMapping{
		{param: "_busy_timeout", pragma: "busy_timeout"},
		{param: "_journal_mode", pragma: "journal_mode"},
		{param: "_synchronous", pragma: "synchronous"},
		{param: "_foreign_keys", pragma: "foreign_keys"},
	} {
		if existing[item.pragma] {
			continue
		}
		if val := strings.TrimSpace(query.Get(item.param)); val != "" {
			additions = append(additions, "_pragma="+item.pragma+"("+val+")")
		}
	}
	if len(additions) == 0 {
		return dsn
	}
	return base + "?" + rawQuery + "&" + strings.Join(additions, "&")
}

func ConfigurePool(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	if db.Dialector.Name() == "postgres" {
		sqlDB.SetMaxOpenConns(30)
		sqlDB.SetMaxIdleConns(10)
		return nil
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	return nil
}
