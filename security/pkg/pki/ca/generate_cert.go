// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Provides utility methods to generate X.509 certificates with different
// options. This implementation is Largely inspired from
// https://golang.org/src/crypto/tls/generate_cert.go.

package ca

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"strings"
	"time"

	"istio.io/istio/security/pkg/pki"
)

// CertOptions contains options for generating a new certificate.
type CertOptions struct {
	// Comma-separated hostnames and IPs to generate a certificate for.
	// This can also be set to the identity running the workload,
	// like kubernetes service account.
	Host string

	// The NotBefore field of the issued certificate.
	NotBefore time.Time

	// TTL of the certificate. NotAfter - NotBefore
	TTL time.Duration

	// Signer certificate (PEM encoded).
	SignerCert *x509.Certificate

	// Signer private key (PEM encoded).
	SignerPriv crypto.PrivateKey

	// Organization for this certificate.
	Org string

	// Whether this certificate should be a Cerificate Authority.
	IsCA bool

	// Whether this cerificate is self-signed.
	IsSelfSigned bool

	// Whether this certificate is for a client.
	IsClient bool

	// Whether this certificate is for a server.
	IsServer bool

	// The size of RSA private key to be generated.
	RSAKeySize int
}

// URIScheme is the URI scheme for Istio identities.
const URIScheme = "spiffe"

// GenCert generates a X.509 certificate and a private key with the given options.
func GenCert(options CertOptions) (pemCert []byte, pemKey []byte, err error) {
	// Generates a RSA private&public key pair.
	// The public key will be bound to the certificate generated below. The
	// private key will be used to sign this certificate in the self-signed
	// case, otherwise the certificate is signed by the signer private key
	// as specified in the CertOptions.
	priv, err := rsa.GenerateKey(rand.Reader, options.RSAKeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("cert generation fails at RSA key generation (%v)", err)
	}
	template, err := genCertTemplate(options)
	if err != nil {
		return nil, nil, fmt.Errorf("cert generation fails at cert template creation (%v)", err)
	}
	signerCert, signerKey := template, crypto.PrivateKey(priv)
	if !options.IsSelfSigned {
		signerCert, signerKey = options.SignerCert, options.SignerPriv
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, template, signerCert, &priv.PublicKey, signerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("cert generation fails at X509 cert creation (%v)", err)
	}

	pemCert, pemKey = encodePem(false, certBytes, priv)
	err = nil
	return
}

// LoadSignerCredsFromFiles loads the signer cert&key from the given files.
//   signerCertFile: cert file name
//   signerPrivFile: private key file name
func LoadSignerCredsFromFiles(signerCertFile string, signerPrivFile string) (*x509.Certificate, crypto.PrivateKey, error) {
	signerCertBytes, err := ioutil.ReadFile(signerCertFile)
	if err != nil {
		return nil, nil, fmt.Errorf("certificate file reading failure (%v)", err)
	}

	signerPrivBytes, err := ioutil.ReadFile(signerPrivFile)
	if err != nil {
		return nil, nil, fmt.Errorf("private key file reading failure (%v)", err)
	}

	cert, err := pki.ParsePemEncodedCertificate(signerCertBytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := pki.ParsePemEncodedKey(signerPrivBytes)
	if err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}

func encodePem(isCSR bool, csrOrCert []byte, priv *rsa.PrivateKey) ([]byte, []byte) {
	encodeMsg := "CERTIFICATE"
	if isCSR {
		encodeMsg = "CERTIFICATE REQUEST"
	}
	csrOrCertPem := pem.EncodeToMemory(&pem.Block{Type: encodeMsg, Bytes: csrOrCert})

	privDer := x509.MarshalPKCS1PrivateKey(priv)
	privPem := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDer})
	return csrOrCertPem, privPem
}

func genSerialNum() (*big.Int, error) {
	serialNumLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNum, err := rand.Int(rand.Reader, serialNumLimit)
	if err != nil {
		return nil, fmt.Errorf("serial number generation failure (%v)", err)
	}
	return serialNum, nil
}

// genCertTemplate generates a certificate template with the given options.
func genCertTemplate(options CertOptions) (*x509.Certificate, error) {
	var keyUsage x509.KeyUsage
	if options.IsCA {
		// If the cert is a CA cert, the private key is allowed to sign other certificate.
		keyUsage = x509.KeyUsageCertSign
	} else {
		// Otherwise the private key is allowed for digital signature and key encipherment.
		keyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}

	extKeyUsages := []x509.ExtKeyUsage{}
	if options.IsServer {
		extKeyUsages = append(extKeyUsages, x509.ExtKeyUsageServerAuth)
	}
	if options.IsClient {
		extKeyUsages = append(extKeyUsages, x509.ExtKeyUsageClientAuth)
	}

	notBefore := options.NotBefore
	// If NotBefore is unset, use current time.
	if notBefore.IsZero() {
		notBefore = time.Now()
	}

	serialNum, err := genSerialNum()
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serialNum,
		Subject: pkix.Name{
			Organization: []string{options.Org},
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(options.TTL),
		KeyUsage:              keyUsage,
		ExtKeyUsage:           extKeyUsages,
		BasicConstraintsValid: true,
	}

	if h := options.Host; len(h) > 0 {
		s, err := buildSubjectAltNameExtension(h)
		if err != nil {
			return nil, err
		}
		template.ExtraExtensions = []pkix.Extension{*s}
	}

	if options.IsCA {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	return template, nil
}

func buildSubjectAltNameExtension(hosts string) (*pkix.Extension, error) {
	ids := []pki.Identity{}
	for _, host := range strings.Split(hosts, ",") {
		if ip := net.ParseIP(host); ip != nil {
			// Use the 4-byte representation of the IP address when possible.
			if eip := ip.To4(); eip != nil {
				ip = eip
			}
			ids = append(ids, pki.Identity{Type: pki.TypeIP, Value: ip})
		} else if strings.HasPrefix(host, URIScheme+":") {
			ids = append(ids, pki.Identity{Type: pki.TypeURI, Value: []byte(host)})
		} else {
			ids = append(ids, pki.Identity{Type: pki.TypeDNS, Value: []byte(host)})
		}
	}

	san, err := pki.BuildSANExtension(ids)
	if err != nil {
		return nil, fmt.Errorf("SAN extension building failure (%v)", err)
	}

	return san, nil
}
