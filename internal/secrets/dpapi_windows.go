//go:build windows

// dpapi_windows.go Windows DPAPI 加解密（crypt32.dll）。
// CryptProtectData 绑定当前 Windows 用户 + 机器，密文无法在其他用户/机器解密，
// 是 Windows 平台存储凭据的推荐方式（等价于 Chromium 保存密码的机制）。
package secrets

import (
	"fmt"
	"syscall"
	"unsafe"
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procProtect   = crypt32.NewProc("CryptProtectData")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree = kernel32.NewProc("LocalFree")
)

func blobToBytes(b dataBlob) []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, (*[1 << 30]byte)(unsafe.Pointer(b.pbData))[:b.cbData])
	return out
}

func bytesToBlob(data []byte) dataBlob {
	if len(data) == 0 {
		return dataBlob{}
	}
	// syscall.BytePtrFromString 不适合二进制；直接取切片底层数组指针。
	// data 在调用期间保持存活（procProtect 调用同步完成），安全。
	return dataBlob{
		cbData: uint32(len(data)),
		pbData: &data[0],
	}
}

func protect(plain []byte) ([]byte, error) {
	in := bytesToBlob(plain)
	var out dataBlob
	// dwFlags = CRYPTPROTECT_UI_FORBIDDEN (0x1)：禁止弹 UI
	r1, _, err := procProtect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // szDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0x1,
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CryptProtectData failed: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

func unprotect(sealed []byte) ([]byte, error) {
	in := bytesToBlob(sealed)
	var out dataBlob
	r1, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0x1, // CRYPTPROTECT_UI_FORBIDDEN
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}
