package transport

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"go.openai.org/api/tunnel-client/pkg/tlsconfig"
)

func TestApplyClientCertificateConfiguresExplicitClientCertificateCallback(t *testing.T) {
	t.Parallel()

	certificate := tls.Certificate{
		Certificate: [][]byte{{1, 2, 3}},
	}
	base := http.DefaultTransport.(*http.Transport).Clone()

	roundTripper, err := ApplyClientCertificate(base, &tlsconfig.ClientCertificate{
		CertPath:    "/tmp/client.pem",
		KeyPath:     "/tmp/client-key.pem",
		Certificate: certificate,
	})
	require.NoError(t, err)

	transport, ok := roundTripper.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	require.Len(t, transport.TLSClientConfig.Certificates, 1)
	require.NotNil(t, transport.TLSClientConfig.GetClientCertificate)

	selected, err := transport.TLSClientConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
	require.NoError(t, err)
	require.Equal(t, certificate.Certificate, selected.Certificate)
}
