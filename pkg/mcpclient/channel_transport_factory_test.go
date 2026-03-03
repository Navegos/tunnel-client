package mcpclient

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/tlsconfig"
	"go.openai.org/api/tunnel-client/pkg/types"
)

func TestChannelTransportFactoryAppliesProxy(t *testing.T) {
	t.Parallel()

	targetCalled := make(chan struct{}, 1)
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalled <- struct{}{}
		http.Error(w, "unexpected direct request", http.StatusBadGateway)
	}))
	t.Cleanup(targetServer.Close)

	proxyCalled := make(chan struct{}, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxyServer.Close)

	proxyURL := mustParseURLFactoryTest(t, proxyServer.URL)
	binding := config.MCPChannelBinding{
		Channel:         types.DefaultChannel,
		TransportKind:   config.MCPTransportHTTPStreamable,
		ServerURL:       mustParseURLFactoryTest(t, targetServer.URL),
		HTTPProxy:       proxyURL,
		HTTPProxySource: config.ProxySource("mcp.server-url"),
	}
	cfg := &config.MCPConfig{
		ChannelBindings: []config.MCPChannelBinding{binding},
	}

	factory, err := newChannelTransportFactory(channelTransportFactoryParams{
		Config:        cfg,
		Logging:       &config.LoggingConfig{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MeterProvider: sdkmetric.NewMeterProvider(),
	})
	if err != nil {
		t.Fatalf("newChannelTransportFactory failed: %v", err)
	}

	client, err := factory.HTTPClientForBinding(binding)
	if err != nil {
		t.Fatalf("HTTPClientForBinding failed: %v", err)
	}
	resp, err := client.Get(targetServer.URL)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case <-proxyCalled:
	default:
		t.Fatalf("expected proxy to receive request")
	}
	select {
	case <-targetCalled:
		t.Fatalf("expected target not to be called directly")
	default:
	}
}

func TestChannelTransportFactoryMTLS(t *testing.T) {
	t.Parallel()

	material := newMTLSTestMaterial(t)

	hit := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{material.serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    material.caPool,
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	bundle := &tlsconfig.Bundle{RootCAs: material.caPool}

	t.Run("request without client certificate fails", func(t *testing.T) {
		binding := config.MCPChannelBinding{
			Channel:       types.DefaultChannel,
			TransportKind: config.MCPTransportHTTPStreamable,
			ServerURL:     mustParseURLFactoryTest(t, server.URL),
		}
		cfg := &config.MCPConfig{ChannelBindings: []config.MCPChannelBinding{binding}}

		factory, err := newChannelTransportFactory(channelTransportFactoryParams{
			Config:        cfg,
			Logging:       &config.LoggingConfig{},
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			MeterProvider: sdkmetric.NewMeterProvider(),
			TLSBundle:     bundle,
		})
		if err != nil {
			t.Fatalf("newChannelTransportFactory failed: %v", err)
		}

		client, err := factory.HTTPClientForBinding(binding)
		if err != nil {
			t.Fatalf("HTTPClientForBinding failed: %v", err)
		}
		resp, err := client.Get(server.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("expected request to fail without client certificate")
		}
	})

	t.Run("request with client certificate succeeds", func(t *testing.T) {
		binding := config.MCPChannelBinding{
			Channel:           types.DefaultChannel,
			TransportKind:     config.MCPTransportHTTPStreamable,
			ServerURL:         mustParseURLFactoryTest(t, server.URL),
			ClientCertificate: material.clientCertificate,
			HTTPProxy:         nil,
			HTTPProxySource:   config.ProxySourceNone,
		}
		cfg := &config.MCPConfig{ChannelBindings: []config.MCPChannelBinding{binding}}

		factory, err := newChannelTransportFactory(channelTransportFactoryParams{
			Config:        cfg,
			Logging:       &config.LoggingConfig{},
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			MeterProvider: sdkmetric.NewMeterProvider(),
			TLSBundle:     bundle,
		})
		if err != nil {
			t.Fatalf("newChannelTransportFactory failed: %v", err)
		}

		client, err := factory.HTTPClientForBinding(binding)
		if err != nil {
			t.Fatalf("HTTPClientForBinding failed: %v", err)
		}
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		select {
		case <-hit:
		default:
			t.Fatalf("expected server handler to be called")
		}
	})
}

type blockingTransportProvider struct {
	started chan struct{}
	release chan struct{}
	count   atomic.Int32
}

func (p *blockingTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportHTTPStreamable
}

func (p *blockingTransportProvider) Build(TransportBuildParams) (mcp.Transport, error) {
	p.count.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return &stubTransport{}, nil
}

func TestChannelTransportFactoryBuildSingleInstanceUnderConcurrency(t *testing.T) {
	t.Parallel()

	binding := config.MCPChannelBinding{
		Channel:       types.DefaultChannel,
		TransportKind: config.MCPTransportHTTPStreamable,
		ServerURL:     mustParseURLFactoryTest(t, "https://example.com"),
	}
	cfg := &config.MCPConfig{
		ChannelBindings: []config.MCPChannelBinding{binding},
	}
	provider := &blockingTransportProvider{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	factory, err := newChannelTransportFactory(channelTransportFactoryParams{
		Config:             cfg,
		Logging:            &config.LoggingConfig{},
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		MeterProvider:      sdkmetric.NewMeterProvider(),
		TransportProviders: []TransportProvider{provider},
	})
	if err != nil {
		t.Fatalf("newChannelTransportFactory failed: %v", err)
	}

	const callers = 8
	results := make([]mcp.Transport, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		index := i
		go func() {
			defer wg.Done()
			transport, err := factory.Build(binding)
			if err != nil {
				t.Errorf("Build failed: %v", err)
				return
			}
			results[index] = transport
		}()
	}

	<-provider.started
	close(provider.release)
	wg.Wait()

	if got := provider.count.Load(); got != 1 {
		t.Fatalf("expected provider to build once, got %d", got)
	}
	if results[0] == nil {
		t.Fatal("expected transport result, got nil")
	}
	if _, ok := results[0].(*stubTransport); !ok {
		t.Fatalf("expected *stubTransport, got %T", results[0])
	}
	for i := 1; i < callers; i++ {
		if results[i] == nil {
			t.Fatalf("expected transport result at %d, got nil", i)
		}
		if results[i] != results[0] {
			t.Fatalf("expected shared transport instance, index %d differed", i)
		}
	}
}

type mtlsTestMaterial struct {
	caPool            *x509.CertPool
	serverCertificate tls.Certificate
	clientCertificate *tlsconfig.ClientCertificate
}

func newMTLSTestMaterial(t *testing.T) mtlsTestMaterial {
	t.Helper()

	caCert, caKey, caPEM := generateCA(t)
	serverCert := generateSignedLeaf(t, caCert, caKey, "mcp-server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	clientCert, clientCertPath, clientKeyPath := generateSignedClientCertificate(t, caCert, caKey)

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(caPEM); !ok {
		t.Fatalf("failed to append CA cert to pool")
	}

	return mtlsTestMaterial{
		caPool:            pool,
		serverCertificate: serverCert,
		clientCertificate: &tlsconfig.ClientCertificate{
			CertPath:    clientCertPath,
			KeyPath:     clientKeyPath,
			Certificate: clientCert,
		},
	}
}

func generateCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, []byte) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(11),
		Subject: pkix.Name{
			CommonName: "test-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if caPEM == nil {
		t.Fatalf("encode CA certificate PEM")
	}
	return caCert, caKey, caPEM
}

func generateSignedLeaf(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey, commonName string, extKeyUsage []x509.ExtKeyUsage) tls.Certificate {
	t.Helper()

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(12),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  extKeyUsage,
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	if leafPEM == nil {
		t.Fatalf("encode leaf certificate PEM")
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})
	if keyPEM == nil {
		t.Fatalf("encode leaf key PEM")
	}
	pair, err := tls.X509KeyPair(leafPEM, keyPEM)
	if err != nil {
		t.Fatalf("load leaf key pair: %v", err)
	}
	return pair
}

func generateSignedClientCertificate(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey) (tls.Certificate, string, string) {
	t.Helper()

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(13),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	clientPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	if clientPEM == nil {
		t.Fatalf("encode client certificate PEM")
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	if keyPEM == nil {
		t.Fatalf("encode client key PEM")
	}
	clientPair, err := tls.X509KeyPair(clientPEM, keyPEM)
	if err != nil {
		t.Fatalf("load client key pair: %v", err)
	}
	dir := t.TempDir()
	clientCertPath := filepath.Join(dir, "client.crt")
	clientKeyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(clientCertPath, clientPEM, 0o600); err != nil {
		t.Fatalf("write client cert: %v", err)
	}
	if err := os.WriteFile(clientKeyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	return clientPair, clientCertPath, clientKeyPath
}

func mustParseURLFactoryTest(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
