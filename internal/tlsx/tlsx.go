// Package tlsx 提供 TLS 支持（需求3：传输过程加密）。
// 未配置证书时可自动生成自签证书（局域网场景，客户端需信任或跳过校验）。
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// EnsureSelfSigned 在 dir 下生成（若不存在）自签证书，返回 cert/key 路径。
// 证书有效期 10 年，SAN 覆盖 localhost、127.0.0.1、::1、0.0.0.0 与本机主机名。
func EnsureSelfSigned(dir string) (certFile, keyFile string, err error) {
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	if fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "FileManage Server", Organization: []string{"FileManage"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), net.ParseIP("0.0.0.0")},
	}
	if h, herr := os.Hostname(); herr == nil {
		tpl.DNSNames = append(tpl.DNSNames, h)
	}
	// 附加本机局域网 IP，便于客户端以 IP 直连 HTTPS
	if addrs, aerr := net.InterfaceAddrs(); aerr == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				tpl.IPAddresses = append(tpl.IPAddresses, ipn.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	cf, err := os.Create(certFile)
	if err != nil {
		return "", "", err
	}
	defer cf.Close()
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return "", "", err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	kf, err := os.OpenFile(keyFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", err
	}
	defer kf.Close()
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
