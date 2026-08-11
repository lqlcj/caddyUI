package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User 是面板管理员。用户名就是邮箱——一个东西同时当登录账号和证书联系邮箱，
// 少填一个格子，也少一个「忘了填联系邮箱」的坑。
type User struct {
	ID        int64
	Username  string
	CreatedAt int64
}

// ErrBadCredentials 邮箱或密码不对。为了不给暴力破解提供信息，账号不存在
// 和密码错误返回的是同一个错误。
var ErrBadCredentials = errors.New("邮箱或密码错误")

// UserCount 返回管理员数量，0 表示还没初始化过。
func (s *Store) UserCount() int {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// emailRe 只做基本形状校验：本地部分@域名，域名至少有一个点。
//
// 故意不追求 RFC 5322 的完整性——那个正则没人看得懂，而且这里的目的不是
// 「判定邮箱真的存在」，是「挡住明显写错的，同时保证能安全地写进 Caddyfile」。
var emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9]([A-Za-z0-9\-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9\-]*[A-Za-z0-9])?)+$`)

// ValidateEmail 检查邮箱格式。
//
// 这个值会原样写进 Caddyfile 的 email 指令当一个 token，所以除了形状之外
// 还必须确保不含空白和大括号——正则里的字符白名单已经覆盖了这一点。
func ValidateEmail(email string) error {
	if email == "" {
		return errors.New("请填写邮箱")
	}
	if len(email) > 254 {
		return errors.New("邮箱太长了")
	}
	if !emailRe.MatchString(email) {
		return errors.New("邮箱格式不正确，应该形如 you@example.com")
	}
	return nil
}

// NormalizeEmail 去空格并转小写。邮箱域名部分本来就大小写无关，本地部分
// 虽然理论上区分，但实际上没有哪家邮箱服务真的区分——统一转小写，
// 省得用户下次登录时大写了一个字母就进不来。
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
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

// CreateUser 新建管理员。username 传邮箱。
func (s *Store) CreateUser(username, password string) (*User, error) {
	username = NormalizeEmail(username)
	if err := ValidateEmail(username); err != nil {
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
			return nil, fmt.Errorf("邮箱 %q 已经注册过了", username)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, CreatedAt: now}, nil
}

// Authenticate 校验账号密码。
//
// 这里对输入做和注册时一样的规范化（去空格、转小写），但不校验格式——
// 老版本用非邮箱用户名注册的账号必须还能登进来。
func (s *Store) Authenticate(username, password string) (*User, error) {
	var (
		u    User
		hash string
	)
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`,
		NormalizeEmail(username)).
		Scan(&u.ID, &u.Username, &hash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// 即使账号不存在也跑一次 bcrypt，避免用响应时间探测账号是否存在。
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
