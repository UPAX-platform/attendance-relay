// Package sig signs relay requests: the fixed RFC 9421 profile UPAX
// verifies, with an RFC 9530 Content-Digest. There is no negotiation; any
// deviation is refused as RELAY_SIGNATURE_INVALID.
//
//	Content-Digest: sha-256=:<base64 SHA-256 of the body>:
//	Signature-Input: upax=("@method" "@authority" "@path" "content-digest");created=<unix>;nonce="<base64 16 bytes>";keyid="<fingerprint>";alg="ed25519"
//	Signature: upax=:<base64 ed25519 signature over the base>:
//
// The signature base is, with no trailing newline:
//
//	"@method": POST
//	"@authority": acme.upax.io
//	"@path": /rly/<id>/sync
//	"content-digest": sha-256=:…:
//	"@signature-params": ("@method" "@authority" "@path" "content-digest");created=…;nonce="…";keyid="…";alg="ed25519"
//
// UPAX carries its own verifier; testdata/vectors.json pins both sides to
// the same bytes. A change here is a change there.
//
// Stdlib only.
package sig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Label is the signature label in Signature-Input and Signature.
const Label = "upax"

// Components is the covered component list, in order.
const Components = `("@method" "@authority" "@path" "content-digest")`

// Window is how far created may be from the verifier's clock.
const Window = 300 * time.Second

// Signer is anything holding the relay key.
type Signer interface {
	Sign(msg []byte) []byte
	Fingerprint() string
}

// Digest is the Content-Digest value for body.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

// Authority is the @authority component of u: the host, lower case, with
// the port only when it isn't the scheme's default.
func Authority(u *url.URL) string {
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

// Params is the signature parameters string (Signature-Input's value
// after "upax=").
func Params(created int64, nonce, keyID string) string {
	return fmt.Sprintf(`%s;created=%d;nonce="%s";keyid="%s";alg="ed25519"`, Components, created, nonce, keyID)
}

// Base is the signature base.
func Base(method, authority, path, digest, params string) string {
	return `"@method": ` + strings.ToUpper(method) + "\n" +
		`"@authority": ` + authority + "\n" +
		`"@path": ` + path + "\n" +
		`"content-digest": ` + digest + "\n" +
		`"@signature-params": ` + params
}

// Nonce returns 16 random bytes, base64.
func Nonce(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Sign adds Content-Digest, Signature-Input and Signature to req, whose
// body is body. created is the (corrected) current time.
func Sign(req *http.Request, body []byte, s Signer, created time.Time, nonce string) {
	digest := Digest(body)
	params := Params(created.Unix(), nonce, s.Fingerprint())
	base := Base(req.Method, Authority(req.URL), req.URL.EscapedPath(), digest, params)
	req.Header.Set("Content-Digest", digest)
	req.Header.Set("Signature-Input", Label+"="+params)
	req.Header.Set("Signature", Label+"=:"+base64.StdEncoding.EncodeToString(s.Sign([]byte(base)))+":")
}

// Verification errors. ErrClockSkew is distinguished because the relay
// corrects its clock offset on it; everything else is one refusal.
var (
	ErrInvalid   = errors.New("sig: signature invalid")
	ErrClockSkew = errors.New("sig: created outside the window")
)

var inputRe = regexp.MustCompile(`^` + Label + `=` + regexp.QuoteMeta(Components) + `;created=(\d+);nonce="([A-Za-z0-9+/]{22}==)";keyid="([0-9A-Z]{26})";alg="ed25519"$`)

// Parsed is a request's signature headers, parsed and shape-checked.
type Parsed struct {
	Created time.Time
	Nonce   string
	KeyID   string
	params  string
	sig     []byte
	digest  string
}

// Parse checks the headers' shape: the exact profile or nothing.
func Parse(h http.Header) (*Parsed, error) {
	m := inputRe.FindStringSubmatch(h.Get("Signature-Input"))
	if m == nil {
		return nil, ErrInvalid
	}
	created, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil, ErrInvalid
	}
	sv, ok := strings.CutPrefix(h.Get("Signature"), Label+"=:")
	if !ok || !strings.HasSuffix(sv, ":") {
		return nil, ErrInvalid
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(sv, ":"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrInvalid
	}
	d := h.Get("Content-Digest")
	if !strings.HasPrefix(d, "sha-256=:") {
		return nil, ErrInvalid
	}
	return &Parsed{Created: time.Unix(created, 0), Nonce: m[2], KeyID: m[3], params: strings.TrimPrefix(h.Get("Signature-Input"), Label+"="), sig: sig, digest: d}, nil
}

// Verify checks a parsed signature against pub for a request to
// method/authority/path, at time now. It does not check the body: see
// VerifyDigest.
func (p *Parsed) Verify(pub ed25519.PublicKey, method, authority, path string, now time.Time) error {
	if d := p.Created.Sub(now); d > Window || d < -Window {
		return ErrClockSkew
	}
	if !ed25519.Verify(pub, []byte(Base(method, authority, path, p.digest, p.params)), p.sig) {
		return ErrInvalid
	}
	return nil
}

// VerifyDigest checks the body against Content-Digest.
func (p *Parsed) VerifyDigest(body []byte) error {
	if subtle.ConstantTimeCompare([]byte(Digest(body)), []byte(p.digest)) != 1 {
		return ErrInvalid
	}
	return nil
}
