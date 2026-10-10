package ec2

import (
	"bytes"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/maycloud/may/internal/awsproto"
)

type keyPair struct {
	id, name, keyType, fingerprint string
	publicKey                      string // OpenSSH authorized_keys format
	created                        time.Time
	tags                           tagMap
}

// OpenSSH wire format helpers (RFC 4251 strings and mpints).

func sshString(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

func sshMpint(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) > 0 && b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return sshString(b)
}

func readSSHString(b []byte) ([]byte, []byte, error) {
	if len(b) < 4 {
		return nil, nil, errors.New("short")
	}
	n := binary.BigEndian.Uint32(b)
	if uint32(len(b)-4) < n {
		return nil, nil, errors.New("short")
	}
	return b[4 : 4+n], b[4+n:], nil
}

func rsaPublicSSH(pub *rsa.PublicKey) []byte {
	var b bytes.Buffer
	b.Write(sshString([]byte("ssh-rsa")))
	b.Write(sshMpint(big.NewInt(int64(pub.E))))
	b.Write(sshMpint(pub.N))
	return b.Bytes()
}

func ed25519PublicSSH(pub ed25519.PublicKey) []byte {
	return append(sshString([]byte("ssh-ed25519")), sshString(pub)...)
}

func authorizedKey(wire []byte, name string) string {
	typ, _, _ := readSSHString(wire)
	return string(typ) + " " + base64.StdEncoding.EncodeToString(wire) + " " + name
}

func colonHex(b []byte) string {
	h := hex.EncodeToString(b)
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// ed25519Private renders an OpenSSH private key ("-----BEGIN OPENSSH
// PRIVATE KEY-----"), the format AWS returns for ED25519 key pairs.
func ed25519Private(priv ed25519.PrivateKey, comment string) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	check := make([]byte, 4)
	_, _ = rand.Read(check)
	var body bytes.Buffer
	body.Write(check)
	body.Write(check)
	body.Write(sshString([]byte("ssh-ed25519")))
	body.Write(sshString(pub))
	body.Write(sshString(priv))
	body.Write(sshString([]byte(comment)))
	for i := byte(1); body.Len()%8 != 0; i++ {
		body.WriteByte(i)
	}
	var b bytes.Buffer
	b.WriteString("openssh-key-v1\x00")
	b.Write(sshString([]byte("none")))
	b.Write(sshString([]byte("none")))
	b.Write(sshString(nil))
	b.Write([]byte{0, 0, 0, 1})
	b.Write(sshString(ed25519PublicSSH(pub)))
	b.Write(sshString(body.Bytes()))
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b.Bytes()})
}

// generateKey returns the private key material, the OpenSSH public key and
// the fingerprint AWS reports for keys it creates.
func generateKey(keyType, name string) (material, public, fingerprint string, err error) {
	switch keyType {
	case "", "rsa":
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", "", "", err
		}
		pkcs8, _ := x509.MarshalPKCS8PrivateKey(k)
		sum := sha1.Sum(pkcs8)
		material = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
		return strings.TrimSpace(material), authorizedKey(rsaPublicSSH(&k.PublicKey), name), colonHex(sum[:]), nil
	case "ed25519":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", "", "", err
		}
		wire := ed25519PublicSSH(pub)
		sum := sha256.Sum256(wire)
		return strings.TrimSpace(string(ed25519Private(priv, name))), authorizedKey(wire, name),
			base64.RawStdEncoding.EncodeToString(sum[:]) + "=", nil
	}
	return "", "", "", errInvalid("Value (%s) for parameter keyType is invalid. Valid values are rsa and ed25519.", keyType)
}

// parsePublicKey accepts an OpenSSH public key and returns its type, the
// normalized authorized_keys line and AWS's fingerprint for imported keys.
func parsePublicKey(material []byte, name string) (keyType, public, fingerprint string, err error) {
	bad := errInvalid("Value for parameter PublicKeyMaterial is invalid. Key is not in valid OpenSSH public key format")
	fields := strings.Fields(string(material))
	if len(fields) < 2 {
		return "", "", "", bad
	}
	wire, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", "", "", bad
	}
	typ, rest, err := readSSHString(wire)
	if err != nil || string(typ) != fields[0] {
		return "", "", "", bad
	}
	switch string(typ) {
	case "ssh-rsa":
		e, rest, err1 := readSSHString(rest)
		n, _, err2 := readSSHString(rest)
		if err1 != nil || err2 != nil {
			return "", "", "", bad
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 1024 {
			return "", "", "", errInvalid("Value for parameter PublicKeyMaterial is invalid. Key size must be at least 1024 bits")
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return "", "", "", bad
		}
		sum := md5.Sum(der)
		return "rsa", authorizedKey(wire, name), colonHex(sum[:]), nil
	case "ssh-ed25519":
		sum := sha256.Sum256(wire)
		return "ed25519", authorizedKey(wire, name), base64.RawStdEncoding.EncodeToString(sum[:]) + "=", nil
	}
	return "", "", "", errInvalid("Value for parameter PublicKeyMaterial is invalid. Only rsa and ed25519 keys are supported")
}

// XML.

type xKeyPair struct {
	KeyPairId      string `xml:"keyPairId"`
	KeyName        string `xml:"keyName"`
	KeyFingerprint string `xml:"keyFingerprint"`
	KeyType        string `xml:"keyType"`
	PublicKey      string `xml:"publicKey,omitempty"`
	CreateTime     string `xml:"createTime"`
	Tags           []xTag `xml:"tagSet>item"`
}

func (k *keyPair) x(includePublic bool) xKeyPair {
	x := xKeyPair{KeyPairId: k.id, KeyName: k.name, KeyFingerprint: k.fingerprint, KeyType: k.keyType,
		CreateTime: isoTime(k.created), Tags: k.tags.list()}
	if includePublic {
		x.PublicKey = k.publicKey
	}
	return x
}

var keyNamePattern = regexp.MustCompile(`^[\x20-\x7e]{1,255}$`)

func (s *Service) registerKeyPairs() {
	s.handle("CreateKeyPair", s.createKeyPair)
	s.handle("ImportKeyPair", s.importKeyPair)
	s.handle("DescribeKeyPairs", s.describeKeyPairs)
	s.handle("DeleteKeyPair", s.deleteKeyPair)
}

func (rs *regionState) checkNewKey(f awsproto.Form) (string, tagMap, error) {
	name := f.Get("KeyName")
	if name == "" {
		return "", nil, errMissing("KeyName")
	}
	if !keyNamePattern.MatchString(name) {
		return "", nil, errInvalid("Value (%s) for parameter keyName is invalid.", name)
	}
	if _, ok := rs.keyPairs[name]; ok {
		return "", nil, ec2Error("InvalidKeyPair.Duplicate", "The keypair already exists")
	}
	if err := checkTagSpecTypes(f, "key-pair"); err != nil {
		return "", nil, err
	}
	tags, err := tagSpecs(f, "key-pair")
	return name, tags, err
}

func (s *Service) createKeyPair(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, tags, err := rs.checkNewKey(f)
	if err != nil {
		return nil, err
	}
	if fmtParam := f.Get("KeyFormat"); fmtParam != "" && fmtParam != "pem" {
		return nil, ec2Error("Unsupported", "may only returns key material in pem format")
	}
	keyType := f.Get("KeyType")
	material, public, fp, err := generateKey(keyType, name)
	if err != nil {
		return nil, err
	}
	if keyType == "" {
		keyType = "rsa"
	}
	k := &keyPair{id: newID("key"), name: name, keyType: keyType, fingerprint: fp, publicKey: public, created: time.Now(), tags: tags}
	rs.keyPairs[name] = k
	return struct {
		KeyName        string `xml:"keyName"`
		KeyFingerprint string `xml:"keyFingerprint"`
		KeyMaterial    string `xml:"keyMaterial"`
		KeyPairId      string `xml:"keyPairId"`
		Tags           []xTag `xml:"tagSet>item"`
	}{name, fp, material, k.id, tags.list()}, nil
}

func (s *Service) importKeyPair(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, tags, err := rs.checkNewKey(f)
	if err != nil {
		return nil, err
	}
	raw := f.Get("PublicKeyMaterial")
	if raw == "" {
		return nil, errMissing("PublicKeyMaterial")
	}
	// The SDKs base64-encode the blob; accept plain text too.
	material, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		material = []byte(raw)
	}
	keyType, public, fp, err := parsePublicKey(material, name)
	if err != nil {
		return nil, err
	}
	k := &keyPair{id: newID("key"), name: name, keyType: keyType, fingerprint: fp, publicKey: public, created: time.Now(), tags: tags}
	rs.keyPairs[name] = k
	return struct {
		KeyName        string `xml:"keyName"`
		KeyFingerprint string `xml:"keyFingerprint"`
		KeyPairId      string `xml:"keyPairId"`
		Tags           []xTag `xml:"tagSet>item"`
	}{name, fp, k.id, tags.list()}, nil
}

func (s *Service) describeKeyPairs(rs *regionState, f awsproto.Form) (interface{}, error) {
	names, ids := idSet(f, "KeyName"), idSet(f, "KeyPairId")
	for n := range names {
		if _, ok := rs.keyPairs[n]; !ok {
			return nil, ec2Error("InvalidKeyPair.NotFound", "The key pair '%s' does not exist", n)
		}
	}
	byID := map[string]*keyPair{}
	for _, k := range rs.keyPairs {
		byID[k.id] = k
	}
	if err := requireIDs(ids, func(id string) bool { return byID[id] != nil }, "InvalidKeyPair.NotFound", "key pair"); err != nil {
		return nil, err
	}
	filters := parseFilters(f)
	includePublic := f.Bool("IncludePublicKey", false)
	out := struct {
		Keys []xKeyPair `xml:"keySet>item"`
	}{}
	for _, n := range sortedKeys(rs.keyPairs) {
		k := rs.keyPairs[n]
		if (names != nil && !names[n]) || (ids != nil && !ids[k.id]) {
			continue
		}
		ok, err := matches(filters, map[string][]string{"key-name": {k.name}, "key-pair-id": {k.id}, "fingerprint": {k.fingerprint},
			"key-type": {k.keyType}}, k.tags)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Keys = append(out.Keys, k.x(includePublic))
		}
	}
	return out, nil
}

// DeleteKeyPair succeeds for missing keys, as in AWS.
func (s *Service) deleteKeyPair(rs *regionState, f awsproto.Form) (interface{}, error) {
	name, id := f.Get("KeyName"), f.Get("KeyPairId")
	if name == "" && id == "" {
		return nil, errMissing("KeyName")
	}
	for n, k := range rs.keyPairs {
		if n == name || k.id == id {
			delete(rs.keyPairs, n)
		}
	}
	return struct {
		Return    bool   `xml:"return"`
		KeyPairId string `xml:"keyPairId,omitempty"`
	}{true, id}, nil
}
