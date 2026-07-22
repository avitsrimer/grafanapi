package config_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generateTestCertKeyPair returns a PEM-encoded self-signed certificate/key pair, suitable for
// both TLS.CAData (as a trust anchor) and TLS.CertData/KeyData (as a client certificate) - the
// tests in this file only care that ToStdTLSConfig parses valid PEM material, not that it chains
// to a real CA.
func generateTestCertKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "grafanapi-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	return certPEM, keyPEM
}

func TestTLS_ToStdTLSConfig_PassesThroughBasicFields(t *testing.T) {
	tlsCfg := &config.TLS{
		Insecure:   true,
		ServerName: "grafana.example.invalid",
		NextProtos: []string{"http/1.1"},
	}

	stdCfg, err := tlsCfg.ToStdTLSConfig()
	require.NoError(t, err)
	assert.True(t, stdCfg.InsecureSkipVerify)
	assert.Equal(t, "grafana.example.invalid", stdCfg.ServerName)
	assert.Equal(t, []string{"http/1.1"}, stdCfg.NextProtos)
	assert.Nil(t, stdCfg.RootCAs)
	assert.Empty(t, stdCfg.Certificates)
}

func TestTLS_ToStdTLSConfig_ValidCAData(t *testing.T) {
	caPEM, _ := generateTestCertKeyPair(t)

	stdCfg, err := (&config.TLS{CAData: caPEM}).ToStdTLSConfig()
	require.NoError(t, err)
	require.NotNil(t, stdCfg.RootCAs)
}

func TestTLS_ToStdTLSConfig_MalformedCAData(t *testing.T) {
	_, err := (&config.TLS{CAData: []byte("this is not PEM-encoded")}).ToStdTLSConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca-data")
}

func TestTLS_ToStdTLSConfig_ValidClientCertificate(t *testing.T) {
	certPEM, keyPEM := generateTestCertKeyPair(t)

	stdCfg, err := (&config.TLS{CertData: certPEM, KeyData: keyPEM}).ToStdTLSConfig()
	require.NoError(t, err)
	require.Len(t, stdCfg.Certificates, 1)
}

func TestTLS_ToStdTLSConfig_MalformedClientCertificate(t *testing.T) {
	_, err := (&config.TLS{CertData: []byte("not a cert"), KeyData: []byte("not a key")}).ToStdTLSConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client certificate")
}

func TestTLS_ToStdTLSConfig_CertDataWithoutKeyDataIsIgnored(t *testing.T) {
	certPEM, _ := generateTestCertKeyPair(t)

	// Only CertData set, no KeyData: ToStdTLSConfig requires both together, so this must not error
	// and must not populate Certificates (a cert with no matching key cannot be used to authenticate).
	stdCfg, err := (&config.TLS{CertData: certPEM}).ToStdTLSConfig()
	require.NoError(t, err)
	assert.Empty(t, stdCfg.Certificates)
}
