// Package store 是面板的持久化层：SQLite + 纯 Go 驱动（不需要 CGO，
// 交叉编译到 arm64 小机器一条命令就行）。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// Store 包装数据库连接。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录: %w", err)
		}
	}

	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 面板的并发量极小，单连接是最省心的选择：彻底没有 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库迁移: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migrations 每一项是一个版本，里面可以有多条语句。只能往后追加，不能改动
// 已发布的条目，否则老用户的库升不上来。
var migrations = [][]string{
	// v1 —— 初始表结构
	{
		`CREATE TABLE users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT    NOT NULL UNIQUE,
			password_hash TEXT    NOT NULL,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE TABLE sessions (
			token      TEXT    PRIMARY KEY,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			csrf       TEXT    NOT NULL,
			expires_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX idx_sessions_expires ON sessions(expires_at)`,
		`CREATE TABLE sites (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			domains         TEXT    NOT NULL,
			upstream_scheme TEXT    NOT NULL DEFAULT 'http',
			upstream_host   TEXT    NOT NULL,
			upstream_port   INTEGER NOT NULL,
			enabled         INTEGER NOT NULL DEFAULT 1,
			https           INTEGER NOT NULL DEFAULT 1,
			force_https     INTEGER NOT NULL DEFAULT 1,
			skip_tls_verify INTEGER NOT NULL DEFAULT 0,
			basic_user      TEXT    NOT NULL DEFAULT '',
			basic_hash      TEXT    NOT NULL DEFAULT '',
			advanced        TEXT    NOT NULL DEFAULT '',
			note            TEXT    NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		)`,
		`CREATE TABLE config_versions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			caddyfile  TEXT    NOT NULL,
			ok         INTEGER NOT NULL DEFAULT 0,
			reason     TEXT    NOT NULL DEFAULT '',
			detail     TEXT    NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	},

	// v2 —— 曾经的端口转发表，功能已移除。
	//
	// 这一格不能删掉、也不能拿去装别的东西：已经升到 v2 的库会记着
	// user_version = 2，改动这里只会让它们跳过后面的迁移。空着就好。
	{},

	// v3 —— 清掉 v2 留下的空表
	{
		`DROP TABLE IF EXISTS forwards`,
	},
}

func migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return err
	}
	for v := current; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range migrations[v] {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("迁移到 v%d 失败: %w", v+1, err)
			}
		}
		// PRAGMA 不支持参数绑定，这里的数字来自代码常量而非用户输入。
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 设置项 ----------

// Setting 读取一个设置，不存在时返回 def。
func (s *Store) Setting(key, def string) string {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

// SetSetting 写入一个设置。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// ---------- 会话 ----------

// Session 是一次登录。
type Session struct {
	Token     string
	UserID    int64
	CSRF      string
	ExpiresAt int64
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateSession 为用户新建一个会话，返回会话本身（含 CSRF token）。
func (s *Store) CreateSession(userID int64, ttl time.Duration) (*Session, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	exp := time.Now().Add(ttl).Unix()
	if _, err := s.db.Exec(
		`INSERT INTO sessions(token, user_id, csrf, expires_at, created_at) VALUES(?,?,?,?,?)`,
		token, userID, csrf, exp, now); err != nil {
		return nil, err
	}
	return &Session{Token: token, UserID: userID, CSRF: csrf, ExpiresAt: exp}, nil
}

// LookupSession 根据 token 找会话，过期的当作不存在。
func (s *Store) LookupSession(token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	var sess Session
	err := s.db.QueryRow(
		`SELECT token, user_id, csrf, expires_at FROM sessions WHERE token = ? AND expires_at > ?`,
		token, time.Now().Unix()).
		Scan(&sess.Token, &sess.UserID, &sess.CSRF, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// DeleteSession 删除一个会话（退出登录）。
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// DeleteUserSessions 踢掉某用户的全部会话（改密码后用）。
func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// CleanupSessions 清掉过期会话。
func (s *Store) CleanupSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}
