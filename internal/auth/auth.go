// Package auth 提供不透明认证凭证与三层认证图（0.2.3，替代 JWT 方案）：
// 客户端通过身份认证后只拿到一个随机 hex 凭证（不编码任何身份信息），
// 真实用户 id/username/role 等一律存服务端认证图（见 center.go），逐请求解析。
// 全链路只持久/只缓存凭证的 SHA-256（HashToken），明文凭证仅签发时下发一次。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// NewCredential 生成 32 字节随机凭证（64 hex，随机哈希形态、无内部结构）
// 及其入库/入缓存哈希。明文只在本次返回值中出现一次。
func NewCredential() (plain, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	plain = hex.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// HashToken 计算凭证的 SHA-256 hex——认证图三层（内存 LRU/redis/持久化）的
// 统一键；明文凭证绝不落盘/落缓存。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomPassword 生成随机访问密码（分享链接默认密码，需求7；
// 兼用于超管账户文件的首启明文口令，需求32/34）。
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
