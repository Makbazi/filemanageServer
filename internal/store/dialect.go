// dialect.go：sqlite3/mysql/postgresql 三驱动共用单一 sqlStore 实现的方言层。
//
// 需求2：查询/upsert/占位符/主键回填全部交给 gorm（见 sqlstore.go/rows.go），
// 本层只保留三件事：
//   - 幂等建表 DDL 清单（dialect_*.go，需求25：sqlite 既有 .db 文件字节兼容，只增不改）；
//   - normalizeDSN：按方言补齐连接参数（sqlite WAL/busy_timeout、mysql parseTime/charset、
//     pg sslmode），旧配置文件里的 DSN 无需改写；
//   - escapeLike：LIKE 模式转义（统一 ESCAPE '/'，gorm 原生不提供该语义）。
package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

type dialect struct {
	name     string   // 规范驱动名（Driver() 返回值）
	ddl      []string // Migrate() 执行的幂等建表语句
	seqReset []string // 跨库复制后的序列对齐（仅 pg；见 copy.go），sqlite/mysql 为 nil
}

// newDialect 按规范驱动名（NormalizeDriver 的输出）构造方言。
func newDialect(name string) (*dialect, error) {
	switch name {
	case DriverSQLite:
		return &dialect{name: DriverSQLite, ddl: sqliteDDL()}, nil
	case DriverMySQL:
		return &dialect{name: DriverMySQL, ddl: mysqlDDL()}, nil
	case DriverPg:
		return &dialect{name: DriverPg, ddl: pgDDL(), seqReset: pgSeqReset()}, nil
	}
	return nil, fmt.Errorf("store: no dialect for driver %q", name)
}

// normalizeDSN 按方言补齐必要参数（幂等，不覆盖用户显式设置）。
func (d *dialect) normalizeDSN(dsn string) string {
	switch d.name {
	case DriverSQLite:
		if !strings.Contains(dsn, "?") {
			dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
		}
	case DriverMySQL:
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		// gorm mysql 驱动扫描 time.Time 依赖 parseTime。
		if !strings.Contains(dsn, "parseTime=") {
			dsn += sep + "parseTime=true"
			sep = "&"
		}
		if !strings.Contains(dsn, "charset=") {
			dsn += sep + "charset=utf8mb4"
		}
	case DriverPg:
		lower := strings.ToLower(dsn)
		if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
			if !strings.Contains(lower, "sslmode=") {
				if strings.Contains(dsn, "?") {
					dsn += "&sslmode=disable"
				} else {
					dsn += "?sslmode=disable"
				}
			}
		} else if !strings.Contains(lower, "sslmode=") {
			dsn += " sslmode=disable"
		}
	}
	return dsn
}

// mysqlUnique 唯一约束冲突（1062）；sqlite/pg 走 gorm TranslateError 或
// sqlstore.isUniqueText 的文本兜底。
func mysqlUnique(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// escapeLike 转义 LIKE 模式中的特殊字符，统一使用 ESCAPE '/'
// （sqlite.go 原实现的 ESCAPE '\' 在 mysql 下是非法字符串转义）。
func escapeLike(s string) string {
	r := strings.NewReplacer("/", "//", "%", "/%", "_", "/_")
	return r.Replace(s)
}
