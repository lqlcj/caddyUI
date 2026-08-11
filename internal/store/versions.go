package store

import (
	"database/sql"
	"errors"
	"time"
)

// ConfigVersion 是一次配置下发的快照。每次改动都存一份，出问题可以一键回滚。
type ConfigVersion struct {
	ID        int64
	Caddyfile string
	OK        bool   // Caddy 是否接受了这份配置
	Reason    string // 触发原因，例如「新增站点 example.com」
	Detail    string // 失败时 Caddy 返回的错误
	CreatedAt int64
}

// CreatedTime 给模板用。
func (c *ConfigVersion) CreatedTime() time.Time {
	return time.Unix(c.CreatedAt, 0)
}

// 保留的历史版本数量。配置文本很小，50 份也就几十 KB。
const keepVersions = 50

// AddConfigVersion 记录一次下发。
func (s *Store) AddConfigVersion(caddyfile string, ok bool, reason, detail string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO config_versions(caddyfile, ok, reason, detail, created_at) VALUES(?,?,?,?,?)`,
		caddyfile, ok, reason, detail, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()

	// 顺手裁掉老版本，但成功的历史至少留着，方便回滚。
	_, _ = s.db.Exec(
		`DELETE FROM config_versions WHERE id NOT IN (
			SELECT id FROM config_versions ORDER BY id DESC LIMIT ?
		 )`, keepVersions)

	return id, nil
}

const versionCols = `id, caddyfile, ok, reason, detail, created_at`

func scanVersion(row interface{ Scan(...any) error }) (*ConfigVersion, error) {
	var v ConfigVersion
	err := row.Scan(&v.ID, &v.Caddyfile, &v.OK, &v.Reason, &v.Detail, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ConfigVersions 返回最近的若干个版本，新的在前。
func (s *Store) ConfigVersions(limit int) ([]*ConfigVersion, error) {
	if limit <= 0 {
		limit = keepVersions
	}
	rows, err := s.db.Query(
		`SELECT `+versionCols+` FROM config_versions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ConfigVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ConfigVersionByID 取单个版本。
func (s *Store) ConfigVersionByID(id int64) (*ConfigVersion, error) {
	return scanVersion(s.db.QueryRow(`SELECT `+versionCols+` FROM config_versions WHERE id = ?`, id))
}

// LastGoodConfig 返回最近一次成功生效的配置，没有则返回 ErrNotFound。
func (s *Store) LastGoodConfig() (*ConfigVersion, error) {
	return scanVersion(s.db.QueryRow(
		`SELECT ` + versionCols + ` FROM config_versions WHERE ok = 1 ORDER BY id DESC LIMIT 1`))
}
