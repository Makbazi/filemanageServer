// Package mailx 出站邮件（0.2.12 邮箱验证码二步验证通道）——纯标准库
// net/smtp + crypto/tls（零新依赖）：SSL=true 隐式 TLS（通常 465）、否则
// STARTTLS（通常 587，服务器不支持则明文拒发——凭证类邮件绝不降级明文）。
// 敏感纪律：口令只在进程内使用，绝不出 API/日志；日志只记收件人域与结果。
package mailx

import (
	"crypto/tls"
	"fmt"
	"net/smtp"
	"os"
	"strings"
	"time"

	"filemanageServer/internal/config"
)

// Send 发送一封 UTF-8 纯文本邮件（subject 单行）。cfg 未配置（Enabled=false）
// 时返回错误——调用方须先判可用并把"未配置"翻译为用户可读文案。
func Send(cfg config.MailCfg, to, subject, body string) error {
	if !cfg.Enabled() {
		return fmt.Errorf("mailx: 邮箱通道未配置")
	}
	if !strings.Contains(to, "@") {
		return fmt.Errorf("mailx: 收件地址非法")
	}
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	msg := build(cfg, to, subject, body)

	if cfg.SSL {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		cli, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			_ = conn.Close()
			return err
		}
		defer func() { _ = cli.Quit() }()
		return sendWith(cli, cfg, auth, to, msg)
	}
	cli, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Quit() }()
	if ok, _ := cli.Extension("STARTTLS"); !ok {
		return fmt.Errorf("mailx: SMTP 服务器不支持 STARTTLS（凭证邮件拒绝明文投递；如为 465 端口请设 mail.ssl=true）")
	}
	if err := cli.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	return sendWith(cli, cfg, auth, to, msg)
}

func sendWith(cli *smtp.Client, cfg config.MailCfg, auth smtp.Auth, to, msg string) error {
	if err := cli.Hello(hostname()); err != nil {
		return err
	}
	if cfg.Username != "" {
		if ok, _ := cli.Extension("AUTH"); ok {
			if err := cli.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := cli.Mail(cfg.From); err != nil {
		return err
	}
	if err := cli.Rcpt(to); err != nil {
		return err
	}
	w, err := cli.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func build(cfg config.MailCfg, to, subject, body string) string {
	from := cfg.From
	if cfg.FromName != "" {
		from = fmt.Sprintf("%s <%s>", cfg.FromName, cfg.From)
	}
	var sb strings.Builder
	sb.WriteString("From: " + from + "\r\n")
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + subject + "\r\n")
	sb.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	sb.WriteString("\r\n")
	return sb.String()
}

func hostname() string {
	// smtp.Hello 用系统主机名即可；失败保持默认（服务器一般不强校验）
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}
