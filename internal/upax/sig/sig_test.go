package sig_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/upax/sig"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

// vector is one entry of testdata/vectors.json, the file UPAX's verifier
// tests read too.
type vector struct {
	Name           string `json:"name"`
	SeedHex        string `json:"seed_hex"`
	PublicKey      string `json:"public_key"`
	Fingerprint    string `json:"fingerprint"`
	Method         string `json:"method"`
	URL            string `json:"url"`
	Body           string `json:"body"`
	Created        int64  `json:"created"`
	Nonce          string `json:"nonce"`
	ContentDigest  string `json:"content_digest"`
	Base           string `json:"signature_base"`
	SignatureInput string `json:"signature_input"`
	Signature      string `json:"signature"`
}

func seed() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

var cases = []struct{ name, url, body string }{
	{"sync", "https://acme.upax.io/rly/01q2a0000000000000000000ab/sync", `{"version":"2.0.0","host_name":"frontdesk-pc","uptime_s":60,"terminals":[]}`},
	{"pair, port in authority", "https://ACME.upax-sb.io:8443/rpair/RLY-7KQ2-M9XD-4TPA", `{"public_key":"x"}`},
	{"empty body, default port dropped", "https://acme.upax.io:443/rly/01q2a0000000000000000000ab/results", ``},
	{"local development", "http://localhost:8300/rly/01q2a0000000000000000000ab/punches", `{"serial":"SIM1","punches":[]}`},
}

func build(t *testing.T, name, url, body string) vector {
	t.Helper()
	k, err := identity.FromSeed(seed())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	created := time.Unix(1793216924, 0)
	nonce := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, 16))
	sig.Sign(req, []byte(body), k, created, nonce)
	return vector{
		Name: name, SeedHex: hex.EncodeToString(seed()), PublicKey: k.PublicBase64(), Fingerprint: k.Fingerprint(),
		Method: "POST", URL: url, Body: body, Created: created.Unix(), Nonce: nonce,
		ContentDigest:  req.Header.Get("Content-Digest"),
		Base:           sig.Base("POST", sig.Authority(req.URL), req.URL.EscapedPath(), req.Header.Get("Content-Digest"), strings.TrimPrefix(req.Header.Get("Signature-Input"), "upax=")),
		SignatureInput: req.Header.Get("Signature-Input"),
		Signature:      req.Header.Get("Signature"),
	}
}

func TestVectors(t *testing.T) {
	var got []vector
	for _, c := range cases {
		got = append(got, build(t, c.name, c.url, c.body))
	}
	path := "testdata/vectors.json"
	if *update {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []vector
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("%d vectors on disk, %d built; run go test -update", len(want), len(got))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("vector %q differs:\n got %+v\nwant %+v", got[i].Name, got[i], want[i])
		}
	}
}

func TestVectorShapes(t *testing.T) {
	v := build(t, "x", cases[0].url, cases[0].body)
	if want := `"@authority": acme.upax.io`; !strings.Contains(v.Base, want) {
		t.Errorf("base lacks %q:\n%s", want, v.Base)
	}
	if strings.HasSuffix(v.Base, "\n") {
		t.Error("base ends with a newline")
	}
	v = build(t, "x", cases[1].url, cases[1].body)
	if want := `"@authority": acme.upax-sb.io:8443`; !strings.Contains(v.Base, want) {
		t.Errorf("base lacks %q:\n%s", want, v.Base)
	}
	v = build(t, "x", cases[2].url, "")
	if v.ContentDigest != "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:" {
		t.Errorf("empty-body digest %s", v.ContentDigest)
	}
}

func TestVerify(t *testing.T) {
	k, _ := identity.FromSeed(seed())
	body := []byte(`{"a":1}`)
	req, _ := http.NewRequest("POST", "https://acme.upax.io/rly/X/sync", bytes.NewReader(body))
	now := time.Now()
	nonce, _ := sig.Nonce(nil)
	sig.Sign(req, body, k, now, nonce)

	p, err := sig.Parse(req.Header)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID != k.Fingerprint() {
		t.Fatalf("keyid %s", p.KeyID)
	}
	if err := p.Verify(k.Public(), "POST", "acme.upax.io", "/rly/X/sync", now); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyDigest(body); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyDigest([]byte(`{"a":2}`)); err != sig.ErrInvalid {
		t.Errorf("tampered body: %v", err)
	}
	if err := p.Verify(k.Public(), "POST", "acme.upax.io", "/rly/Y/sync", now); err != sig.ErrInvalid {
		t.Errorf("other path: %v", err)
	}
	if err := p.Verify(k.Public(), "POST", "evil.example", "/rly/X/sync", now); err != sig.ErrInvalid {
		t.Errorf("other authority: %v", err)
	}
	if err := p.Verify(k.Public(), "POST", "acme.upax.io", "/rly/X/sync", now.Add(6*time.Minute)); err != sig.ErrClockSkew {
		t.Errorf("skew: %v", err)
	}
	other, _ := identity.Generate()
	if err := p.Verify(other.Public(), "POST", "acme.upax.io", "/rly/X/sync", now); err != sig.ErrInvalid {
		t.Errorf("other key: %v", err)
	}
	h := req.Header.Clone()
	h.Set("Signature-Input", strings.Replace(h.Get("Signature-Input"), `alg="ed25519"`, `alg="rsa-pss-sha512"`, 1))
	if _, err := sig.Parse(h); err != sig.ErrInvalid {
		t.Errorf("other alg: %v", err)
	}
}
