package azure

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
)

// unusableSSHKey returns a fresh public key in authorized_keys form whose private key is discarded at once. Azure
// requires a login on Linux VMs; this one can never be used.
func unusableSSHKey() (string, error) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	const algorithm = "ssh-ed25519"
	blob := make([]byte, 0, 4+len(algorithm)+4+len(public))
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(algorithm)))
	blob = append(blob, algorithm...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(public)))
	blob = append(blob, public...)
	return algorithm + " " + base64.StdEncoding.EncodeToString(blob), nil
}
