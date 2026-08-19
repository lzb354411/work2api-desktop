//go:build !windows

// dpapi_fallback.go 非 Windows 平台：DPAPI 不可用，退化为 base64。
// 桌面版主要目标平台是 Windows；此 fallback 保证代码可跨平台编译与测试。
package secrets

import (
	"encoding/base64"
	"errors"
)

func protect(plain []byte) ([]byte, error) {
	return []byte(base64.StdEncoding.EncodeToString(plain)), nil
}

func unprotect(sealed []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(string(sealed))
	if err != nil {
		return nil, errors.New("secrets: base64 decode failed (non-windows fallback)")
	}
	return raw, nil
}
