package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// User 是面板管理员。
type User struct {
	ID        int64
	Username  string
	CreatedAt int64
}

// ErrBadCredentials 用户名或密码不对。为了不给暴力破解提供信息，用户名不存在
// 和密码错误返回的是同一个错误。
var ErrBadCredentials = errors.New("用户名或密码错误")

// UserCount 返回管理员数量，0 表示还没初始化过。
func (s *Store) UserCount() int {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ValidateUsername 检查用户名是否合法。
func ValidateUsername(name string) error {
	if len(name) < 3 || len(name) > 32 {
		return errors.New("用户名长度需要在 3~32 个字符之间")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return errors.New("用户名只能包含字母、数字、下划线、短横线和点")
		}
	}
	return nil
}

// ValidatePassword 检查密码强度。故意定得很宽松——这是个人自用面板，
// 门槛太高反而逼人用弱密码加便签。
func ValidatePassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("密码至少 8 位")
	}
	if len(pw) > 72 {
		// bcrypt 只处理前 72 字节，超过部分被静默丢弃，不如直接拒绝。
		return errors.New("密码不能超过 72 个字符")
	}
	return nil
}

// HashPassword 生成 bcrypt 哈希。Caddy 的 basic_auth 用的也是 bcrypt，
// 所以这一个函数两处复用。
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CreateUser 新建管理员。
func (s *Store) CreateUser(username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO users(username, password_hash, created_at, updated_at) VALUES(?,?,?,?)`,
		username, hash, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("用户名 %q 已存在", username)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, CreatedAt: now}, nil
}

// Authenticate 校验用户名密码。
func (s *Store) Authenticate(username, password string) (*User, error) {
	var (
		u    User
		hash string
	)
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`,
		strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &hash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// 即使用户不存在也跑一次 bcrypt，避免用响应时间探测用户名是否存在。
		bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"),
			[]byte(password))
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrBadCredentials
	}
	return &u, nil
}

// UserByID 按 ID 取用户。
func (s *Store) UserByID(id int64) (*User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT id, username, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ChangePassword 修改密码，需要提供旧密码。
func (s *Store) ChangePassword(id int64, oldPw, newPw string) error {
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPw)) != nil {
		return errors.New("当前密码不正确")
	}
	if err := ValidatePassword(newPw); err != nil {
		return err
	}
	newHash, err := HashPassword(newPw)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		newHash, time.Now().Unix(), id)
	return err
}
