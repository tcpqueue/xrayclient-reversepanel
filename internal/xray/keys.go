package xray

// 密钥生成，与官方 `xray uuid` / `xray x25519` / `xray vlessenc` 输出格式一致。
// 只用 Go 标准库（crypto/ecdh、crypto/mlkem），不额外引入依赖。

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/xtls/xray-core/common/uuid"
)

var b64 = base64.RawURLEncoding

// genX25519 返回 (私钥, 公钥/password)，私钥按 https://cr.yp.to/ecdh.html 做 clamp，与 xray x25519 一致。
func genX25519() (string, string, error) {
	priv := make([]byte, 32)
	rand.Read(priv)
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	return b64.EncodeToString(priv), b64.EncodeToString(key.PublicKey().Bytes()), nil
}

// genVLESSEnc 生成一对 decryption（服务端）/ encryption（客户端）。
// auth: "x25519"（默认）或 "mlkem768"（后量子认证）；mode: native / xorpub / random。
func genVLESSEnc(auth, mode string) (decryption, encryption string, err error) {
	switch mode {
	case "":
		mode = "native"
	case "native", "xorpub", "random":
	default:
		return "", "", fmt.Errorf("mode 只能是 native/xorpub/random")
	}
	var serverKey, clientKey string
	switch auth {
	case "", "x25519":
		serverKey, clientKey, err = genX25519()
		if err != nil {
			return
		}
	case "mlkem768":
		seed := make([]byte, mlkem.SeedSize)
		rand.Read(seed)
		dk, e := mlkem.NewDecapsulationKey768(seed)
		if e != nil {
			return "", "", e
		}
		serverKey, clientKey = b64.EncodeToString(seed), b64.EncodeToString(dk.EncapsulationKey().Bytes())
	default:
		return "", "", fmt.Errorf("auth 只能是 x25519 或 mlkem768")
	}
	decryption = strings.Join([]string{"mlkem768x25519plus", mode, "600s", serverKey}, ".")
	encryption = strings.Join([]string{"mlkem768x25519plus", mode, "0rtt", clientKey}, ".")
	return
}

func Generate(kind, auth, mode string) (map[string]string, error) {
	switch kind {
	case "uuid":
		u := uuid.New()
		return map[string]string{"uuid": u.String()}, nil
	case "x25519":
		priv, pub, err := genX25519()
		if err != nil {
			return nil, err
		}
		return map[string]string{"privateKey": priv, "password": pub}, nil
	case "shortid":
		b := make([]byte, 8)
		rand.Read(b)
		return map[string]string{"shortId": hex.EncodeToString(b)}, nil
	case "vlessenc":
		d, e, err := genVLESSEnc(auth, mode)
		if err != nil {
			return nil, err
		}
		return map[string]string{"decryption": d, "encryption": e}, nil
	}
	return nil, fmt.Errorf("未知类型 %q", kind)
}
