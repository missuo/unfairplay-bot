package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strconv"
	"time"
)

// Signer issues download links bound to one artifact and an expiry time.
type Signer struct {
	key  []byte
	base string
}

func (s Signer) mac(id string, exp int64) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("unfairplay-bot/download/v1\n" + id + "\n" + strconv.FormatInt(exp, 10)))
	return m.Sum(nil)
}

func (s Signer) URL(id, fileName string, exp time.Time) string {
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp.Unix(), 10))
	q.Set("sig", base64.RawURLEncoding.EncodeToString(s.mac(id, exp.Unix())))
	return s.base + "/d/" + id + "/" + url.PathEscape(fileName) + "?" + q.Encode()
}

// Verify checks the signature and that the link has not expired.
func (s Signer) Verify(id, expText, sig string, now time.Time) bool {
	exp, err := strconv.ParseInt(expText, 10, 64)
	if err != nil || now.Unix() >= exp {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	return err == nil && hmac.Equal(got, s.mac(id, exp))
}
