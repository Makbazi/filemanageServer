// totp.go：RFC 6238 TOTP（HMAC-SHA1 / 30s 步长 / 6 位）——0.2.12 二次验证
// TOTP 通道，纯标准库实现（零新依赖，需求29 编译即验证）。密钥为 160-bit
// 随机数的 base32 编码（RFC 4226 推荐强度，兼容主流验证器 App）。
// 校验窗口 ±1 步（90 秒容忍时钟漂移）；同一密钥同一时间步的重放由调用侧
// 防爆破守卫（bruteGuard）兜底——TOTP 本身无状态。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	stderrors "errors"
	"net/url"
	"strings"
	"time"
)

const (
	totpDigits    = 6
	totpPeriodSec = 30 // 未定型常量：Unix()/totpPeriodSec 两侧自动适配
	totpSkew      = 1  // ±1 步
)

var totpSecretEnc = base32.StdEncoding // 字母表 A-Z2-7（RFC 4648；填充去除）

// GenerateTotpSecret 生成新的 TOTP 密钥（20 字节=160-bit，base32 无填充）。
func GenerateTotpSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.TrimRight(totpSecretEnc.EncodeToString(b), "="), nil
}

func decodeTotpSecret(secret string) ([]byte, error) {
	return totpSecretEnc.DecodeString(strings.ToUpper(strings.TrimRight(strings.TrimSpace(secret), "=")))
}

var errTotpKey = stderrors.New("auth: empty totp key")

func totpAt(key []byte, counter int64) (int64, error) {
	if len(key) == 0 {
		return 0, errTotpKey
	}
	msg := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		msg[i] = byte(counter & 0xff)
		counter >>= 8
	}
	mac := hmac.New(sha1.New, key)
	mac.Write(msg)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (int64(sum[off]&0x7f) << 24) | int64(sum[off+1])<<16 | int64(sum[off+2])<<8 | int64(sum[off+3])
	return bin % 1000000, nil
}

// TotpCode 计算 now 时刻的 6 位码（左补零；自助开通的确认流程回显用）。
func TotpCode(secret string, now time.Time) (string, error) {
	key, err := decodeTotpSecret(secret)
	if err != nil {
		return "", err
	}
	code, err := totpAt(key, now.Unix()/totpPeriodSec)
	if err != nil {
		return "", err
	}
	return pad6(code), nil
}

// ValidateTotp 校验用户输入码（接受 ±1 步时钟漂移；恒定时间比较）。
func ValidateTotp(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	key, err := decodeTotpSecret(secret)
	if err != nil || len(key) == 0 {
		return false
	}
	counter := now.Unix() / totpPeriodSec
	want := []byte(code)
	for d := -int64(totpSkew); d <= totpSkew; d++ {
		c, err := totpAt(key, counter+d)
		if err != nil {
			return false
		}
		if hmac.Equal([]byte(pad6(c)), want) {
			return true
		}
	}
	return false
}

func pad6(v int64) string {
	b := []byte("000000")
	for i := 5; i >= 0 && v > 0; i-- {
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b)
}

// TotpURI 构造 otpauth:// URI（RFC 同源格式；验证器 App 手工录入/导入用，
// 服务端不出二维码图——见 CHANGELOG 决策记录）。
func TotpURI(account, secret string) string {
	return "otpauth://totp/" + url.PathEscape("FileManage:"+account) +
		"?secret=" + secret +
		"&issuer=" + url.QueryEscape("FileManage") +
		"&algorithm=SHA1&digits=6&period=30"
}
