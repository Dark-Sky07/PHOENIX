package pppvpn

import "testing"

func TestFingerprintDistinguishesCredentialBoundaries(t *testing.T) {
	one := Instance{Protocol: PPTP, Credentials: []Credential{{Username: "a", Password: "p\x01b\x00q"}}}
	two := Instance{Protocol: PPTP, Credentials: []Credential{{Username: "a", Password: "p"}, {Username: "b", Password: "q"}}}
	if one.fingerprint() == two.fingerprint() {
		t.Fatal("different PPP credential lists have the same fingerprint")
	}
}
