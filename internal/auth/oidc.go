// oidc.go：0.2.12 OAuth2/OIDC 支撑原语——最小 id_token（JWS HS256，标准库
// 手工签发，不引回 golang-jwt：0.2.3 去 JWT 的红线是"凭证不编码身份"，
// 本 token 只是 OIDC 协议的**声明回执**，一切访问鉴权仍走认证图查表，
// id_token 不作为凭证使用——第一方客户端不验签，第三方集成可持密钥验证）。
//
// 签名密钥 = <dataDir>/oidc.key（0600，64-hex 随机；生成/复用形态对齐
// superadmin 账户文件与旧 jwt.secret——多实例共享 dataDir 即共享密钥）。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// b64url JWS Base64url（无填充）。
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// LoadOrCreateOidcKey 读取签名密钥；文件缺失则生成 32 字节随机密钥并持久化
// （0600；目录自动创建）。返回 32 字节 HMAC key。
func LoadOrCreateOidcKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		k, derr := hex.DecodeString(trimKey(b))
		if derr == nil && len(k) == 32 {
			return k, nil
		}
		return nil, fmt.Errorf("auth: oidc key file %s 形态非法（期望 64-hex）", path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return buf, nil
}

func trimKey(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

// SignIDToken 签发 HS256 JWT（header {"alg":"HS256","typ":"JWT"}）。
// claims 由调用方组装（iss/sub/aud/exp/iat/auth_time/nonce/amr…）——
// sub=账户名（用户端口数字 id 永不可见纪律的延伸，绝不用数字 id 当 sub）。
func SignIDToken(key []byte, claims map[string]any) (string, error) {
	head, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64url(head) + "." + b64url(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	return signing + "." + b64url(mac.Sum(nil)), nil
}

// OidcNow 时间戳辅助（claims 统一秒级 iat/exp/auth_time）。
func OidcNow() int64 { return time.Now().Unix() }
