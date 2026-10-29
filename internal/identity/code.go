package identity

import (
	"fmt"
	"strings"
)

// PairingCode normalises a pairing code typed by an operator to the form
// UPAX shows and expects in /rpair/{code}: "RLY-XXXX-XXXX-XXXX", 12
// characters from Alphabet. Case, spaces and hyphens are ignored, the
// "RLY" prefix is optional, and look-alikes are mapped (I, L → 1; O → 0).
func PairingCode(s string) (string, error) {
	t := strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(s)))
	t = strings.TrimPrefix(t, "RLY")
	if len(t) != 12 {
		return "", fmt.Errorf("a pairing code looks like RLY-XXXX-XXXX-XXXX (12 letters and digits); got %q", s)
	}
	var b strings.Builder
	b.WriteString("RLY")
	for i, r := range t {
		switch r {
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(Alphabet, r) {
			return "", fmt.Errorf("pairing code %q has an invalid character %q", s, r)
		}
		if i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}
