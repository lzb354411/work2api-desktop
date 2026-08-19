// Package secrets 提供敏感数据的静态加密存储。
//
// Windows 上使用 DPAPI（CryptProtectData，用户级密钥）加密，
// 非 Windows 平台退化为 Base64 明文（带标记头，便于跨平台识别）。
//
// 存储格式：magic 前缀 "W2AE1\x00" + 密文。
// - Windows: DPAPI blob
// - 其他平台: base64(原文)
package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// magic 标识加密块格式版本。
var magic = []byte("W2AE1\x00")

// Seal 加密明文。
func Seal(plain []byte) ([]byte, error) {
	enc, err := protect(plain)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return append(append([]byte{}, magic...), enc...), nil
}

// Open 解密 Seal 产物。兼容识别明文 Base64（迁移旧数据用）。
func Open(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, errors.New("secrets: empty input")
	}
	if bytes.HasPrefix(sealed, magic) {
		return unprotect(sealed[len(magic):])
	}
	// 无 magic：尝试 base64 明文（旧格式），失败则报错
	if raw, err := base64.StdEncoding.DecodeString(string(sealed)); err == nil {
		return raw, nil
	}
	return nil, errors.New("secrets: unrecognized format")
}

// SealFile 以 0600 权限原子写入加密文件（tmp + rename）。
func SealFile(path string, plain []byte) error {
	sealed, err := Seal(plain)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// OpenFile 读取并解密文件；文件不存在返回 os.ErrNotExist。
func OpenFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Open(raw)
}
