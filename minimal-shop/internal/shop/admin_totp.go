package shop

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() (string, error) {
	var raw [20]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	return totpEncoding.EncodeToString(raw[:]), nil
}

func (a *App) EnrollAdminTOTP(ctx context.Context, adminID int64, secret string) error {
	if adminID <= 0 {
		return ErrInvalid
	}
	raw, err := totpEncoding.DecodeString(secret)
	if err != nil || len(raw) < 20 {
		return ErrInvalid
	}
	nonce := make([]byte, a.cipher.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := a.cipher.aead.Seal(nil, nonce, []byte(secret), []byte("admin-totp:"+strconv.FormatInt(adminID, 10)))
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE users SET totp_nonce=$1,totp_ciphertext=$2 WHERE id=$3 AND role='admin' AND totp_ciphertext IS NULL`, nonce, ciphertext, adminID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrInvalid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events (actor_user_id,action,target_type,target_id) VALUES ($1,'admin.totp.enroll','user',$2)`, adminID, strconv.FormatInt(adminID, 10)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *App) adminTOTPSecret(adminID int64, nonce, ciphertext []byte) (string, error) {
	if len(nonce) == 0 || len(ciphertext) == 0 {
		return "", ErrForbidden
	}
	plain, err := a.cipher.aead.Open(nil, nonce, ciphertext, []byte("admin-totp:"+strconv.FormatInt(adminID, 10)))
	if err != nil {
		return "", ErrForbidden
	}
	return string(plain), nil
}

func validTOTP(secret, code string, now time.Time) (int64, bool) {
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		return 0, false
	}
	key, err := totpEncoding.DecodeString(secret)
	if err != nil || len(key) < 20 {
		return 0, false
	}
	current := now.Unix() / 30
	for step := current - 1; step <= current+1; step++ {
		candidate := totpCode(key, step)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

func totpCode(key []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key) // RFC 6238 TOTP uses HMAC-SHA1.
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := int(sum[len(sum)-1] & 0x0f)
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}
