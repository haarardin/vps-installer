package app

import "bytes"

func bytesReplace(b, secret []byte) []byte {
	if len(secret) == 0 {
		return b
	}
	return bytes.ReplaceAll(b, secret, []byte("[REDACTED]"))
}
