package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// uploadSigQueryParam and uploadExpQueryParam name the query parameters that
// carry an HMAC-signed upload URL. They are paired: a URL with one but not
// the other is rejected by VerifyUploadSignature.
const (
	uploadSigQueryParam = "sig"
	uploadExpQueryParam = "exp"
)

// UploadSignatureTTL is how long an HMAC-signed upload URL stays valid. The
// "Get full quality" flow asks the device to upload tens to hundreds of MB
// over a phone tether, so this is generous; the device's own retry budget
// caps the practical lifetime well below this.
const UploadSignatureTTL = 24 * time.Hour

// SignUploadPath returns the (exp, sig) pair that authenticates a PUT to
// urlPath. urlPath should already include the leading slash (e.g.
// "/upload/<dongle>/<route>/<segment>/<filename>") because the signature
// binds the exact path the upload endpoint will see, leaving no room for
// a caller to redirect bytes to a different file.
//
// The returned signature is URL-safe base64 (no padding); the caller is
// expected to attach it as a query parameter via AppendUploadSignature.
func SignUploadPath(secret []byte, urlPath string, exp time.Time) string {
	payload := fmt.Sprintf("PUT\n%s\n%d", urlPath, exp.Unix())
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// AppendUploadSignature returns rawURL with ?exp=...&sig=... appended (or
// merged into the existing query string). It is a thin convenience over the
// net/url package so call sites can stay one-liners.
func AppendUploadSignature(rawURL string, exp time.Time, sig string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := parsed.Query()
	q.Set(uploadExpQueryParam, strconv.FormatInt(exp.Unix(), 10))
	q.Set(uploadSigQueryParam, sig)
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

// VerifyUploadSignature checks whether the request's exp+sig query params
// authorise a PUT to urlPath under secret. urlPath must be the same path
// that was signed (typically c.Request().URL.Path). Returns true when the
// signature matches and the expiry has not passed; false otherwise.
//
// The function is strict: an empty secret, missing query params, or a
// mismatched signature all fail closed. Callers should treat it as the
// only gate the upload route enforces when bypassing JWT auth.
func VerifyUploadSignature(secret []byte, urlPath, expParam, sigParam string) bool {
	return verifyUploadSignatureAt(secret, urlPath, expParam, sigParam, time.Now())
}

// verifyUploadSignatureAt is the testable seam for VerifyUploadSignature.
func verifyUploadSignatureAt(secret []byte, urlPath, expParam, sigParam string, now time.Time) bool {
	if len(secret) == 0 || expParam == "" || sigParam == "" {
		return false
	}
	expUnix, err := strconv.ParseInt(expParam, 10, 64)
	if err != nil {
		return false
	}
	if now.After(time.Unix(expUnix, 0)) {
		return false
	}
	expected := SignUploadPath(secret, urlPath, time.Unix(expUnix, 0))
	expectedBytes, err := base64.RawURLEncoding.DecodeString(expected)
	if err != nil {
		return false
	}
	gotBytes, err := base64.RawURLEncoding.DecodeString(sigParam)
	if err != nil {
		return false
	}
	return hmac.Equal(expectedBytes, gotBytes)
}
