package service

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type RedeemCode struct {
	ID        int64
	Code      string
	Type      string
	Value     float64
	Status    string
	UsedBy    *int64
	UsedAt    *time.Time
	Notes     string
	CreatedAt time.Time
	ExpiresAt *time.Time

	GroupID      *int64
	ValidityDays int

	User  *User
	Group *Group
}

func (r *RedeemCode) IsUsed() bool {
	return r.Status == StatusUsed
}

func (r *RedeemCode) IsExpired() bool {
	return r.IsExpiredAt(time.Now())
}

func (r *RedeemCode) IsExpiredAt(now time.Time) bool {
	if r == nil {
		return false
	}
	if r.Status == StatusExpired {
		return true
	}
	return r.Status == StatusUnused && r.ExpiresAt != nil && !r.ExpiresAt.After(now)
}

func (r *RedeemCode) CanUse() bool {
	return r.Status == StatusUnused && !r.IsExpired()
}

func GenerateRedeemCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// invitationCodeAlphabet 去掉易混字符 0/O/1/I/L，方便用户口述/手输。
const invitationCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// invitationCodeLength 邀请码只用于注册门槛（非金额、有频控），取 6 位易读短码。
const invitationCodeLength = 6

// GenerateInvitationCode 生成 6 位大写字母+数字邀请码（如 K7M2X9）。
func GenerateInvitationCode() (string, error) {
	b := make([]byte, invitationCodeLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, invitationCodeLength)
	for i := range b {
		out[i] = invitationCodeAlphabet[int(b[i])%len(invitationCodeAlphabet)]
	}
	return string(out), nil
}
