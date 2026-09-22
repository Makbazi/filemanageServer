// Package auth 提供 JWT 访问令牌与 refresh token 工具（需求22）。
// 访问令牌短期有效（默认 1h）；refresh 令牌入库（只存 SHA-256），
// 活跃使用自动轮换续期，超过闲置上限（默认 21 天 = 3 周）需重新登录。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// IssueAccess 签发短期访问令牌。
func IssueAccess(secret string, userID int64, username, role string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":  userID,
		"name": username,
		"role": role,
		"iat":  now.Unix(),
		"exp":  now.Add(ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// Claims 访问令牌声明（0.1.0：替代 ParseAccess 的返回值，支持超管令牌）。
type Claims struct {
	UID      int64 // DB 用户 ID；超管令牌恒为 0（store.SuperAdminID）
	Username string
	Role     string
	Super    bool  // 超管令牌（internal/superadmin，需求32）：不走数据库
	Gen      int64 // 超管凭据纪元（口令轮换/文件重置即吊销旧令牌）
}

// ParseAccessClaims 校验访问令牌并返回声明。
func ParseAccessClaims(secret, ts string) (*Claims, error) {
	tok, err := jwt.Parse(ts, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	c := &Claims{}
	c.Username, _ = claims["name"].(string)
	c.Role, _ = claims["role"].(string)
	c.Super, _ = claims["super"].(bool)
	if f, ok := claims["sub"].(float64); ok {
		c.UID = int64(f)
	}
	if g, ok := claims["gen"].(float64); ok {
		c.Gen = int64(g)
	}
	if !c.Super && c.UID <= 0 {
		return nil, errors.New("invalid subject")
	}
	return c, nil
}

// IssueSuperAccess 签发超管访问令牌（0.1.0 需求32/36，仅管理端口可用）：
// sub=0 + super=true + 当前凭据纪元；authCtx 逐请求核对纪元，
// 口令变更/文件重置（generation++）即吊销全部已发令牌。
func IssueSuperAccess(secret, username string, gen int64, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":  int64(0),
		"name": username,
		"role": "admin",
		// super/gen：超管令牌标记与凭据纪元（需求32/34）
		"super": true,
		"gen":   gen,
		"iat":   now.Unix(),
		"exp":   now.Add(ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// NewRefreshToken 生成 refresh token（明文仅下发一次）及其入库哈希。
func NewRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken 计算 token 的 SHA-256 hex（数据库只存哈希）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueShareToken 为通过密码校验的分享链接签发短期访问令牌（需求7）。
func IssueShareToken(secret, linkToken string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"share": linkToken,
		"iat":   now.Unix(),
		"exp":   now.Add(ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// ParseShareToken 校验分享访问令牌，返回其绑定的链接 token。
func ParseShareToken(secret, ts, expectLinkToken string) error {
	tok, err := jwt.Parse(ts, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !tok.Valid {
		return errors.New("invalid share token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return errors.New("invalid claims")
	}
	if s, _ := claims["share"].(string); s != expectLinkToken {
		return errors.New("share token mismatch")
	}
	return nil
}

// RandomPassword 生成随机访问密码（分享链接默认密码，需求7；
// 0.1.0 起兼用于超管账户文件的首启明文口令，需求32/34）。
func RandomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	if n <= 0 {
		n = 10
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}
