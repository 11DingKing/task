package taskfile

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPNode_CacheKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		entrypoint  string
		expectedKey string
	}{
		{
			entrypoint:  "https://github.com",
			expectedKey: "http.github.com..996e1f714b08e971ec79e3bea686287e66441f043177999a13dbc546d8fe402a",
		},
		{
			entrypoint:  "https://github.com/Taskfile.yml",
			expectedKey: "http.github.com.Taskfile.yml.85b3c3ad71b78dc74e404c7b4390fc13672925cb644a4d26c21b9f97c17b5fc0",
		},
		{
			entrypoint:  "https://github.com/foo",
			expectedKey: "http.github.com.foo.df3158dafc823e6847d9bcaf79328446c4877405e79b100723fa6fd545ed3e2b",
		},
		{
			entrypoint:  "https://github.com/foo/Taskfile.yml",
			expectedKey: "http.github.com.foo.Taskfile.yml.aea946ea7eb6f6bb4e159e8b840b6b50975927778b2e666df988c03bbf10c4c4",
		},
		{
			entrypoint:  "https://github.com/foo/bar",
			expectedKey: "http.github.com.foo.bar.d3514ad1d4daedf9cc2825225070b49ebc8db47fa5177951b2a5b9994597570c",
		},
		{
			entrypoint:  "https://github.com/foo/bar/Taskfile.yml",
			expectedKey: "http.github.com.bar.Taskfile.yml.b9cf01e01e47c0e96ea536e1a8bd7b3a6f6c1f1881bad438990d2bfd4ccd0ac0",
		},
	}

	for _, tt := range tests {
		node, err := NewHTTPNode(tt.entrypoint, "", false)
		require.NoError(t, err)
		key := node.CacheKey()
		assert.Equal(t, tt.expectedKey, key)
	}
}

func TestBuildHTTPClient_Default(t *testing.T) {
	t.Parallel()

	// When no TLS customization is needed, should return http.DefaultClient
	client, err := buildHTTPClient(false, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, http.DefaultClient, client)
}

func TestBuildHTTPClient_Insecure(t *testing.T) {
	t.Parallel()

	client, err := buildHTTPClient(true, "", "", "")
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.NotEqual(t, http.DefaultClient, client)

	// Check that InsecureSkipVerify is set
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
}

func TestBuildHTTPClient_CACert(t *testing.T) {
	t.Parallel()

	// Create a temporary CA cert file
	tempDir := t.TempDir()
	caCertPath := filepath.Join(tempDir, "ca.crt")

	// Generate a valid CA certificate
	caCertPEM := generateTestCACert(t)
	err := os.WriteFile(caCertPath, caCertPEM, 0o600)
	require.NoError(t, err)

	client, err := buildHTTPClient(false, caCertPath, "", "")
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.NotEqual(t, http.DefaultClient, client)

	// Check that custom RootCAs is set
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	assert.NotNil(t, transport.TLSClientConfig.RootCAs)
}

func TestBuildHTTPClient_CACertNotFound(t *testing.T) {
	t.Parallel()

	client, err := buildHTTPClient(false, "/nonexistent/ca.crt", "", "")
	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "failed to read CA certificate")
}

func TestBuildHTTPClient_CACertInvalid(t *testing.T) {
	t.Parallel()

	// Create a temporary file with invalid content
	tempDir := t.TempDir()
	caCertPath := filepath.Join(tempDir, "invalid.crt")
	err := os.WriteFile(caCertPath, []byte("not a valid certificate"), 0o600)
	require.NoError(t, err)

	client, err := buildHTTPClient(false, caCertPath, "", "")
	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "failed to parse CA certificate")
}

func TestBuildHTTPClient_CertWithoutKey(t *testing.T) {
	t.Parallel()

	client, err := buildHTTPClient(false, "", "/path/to/cert.crt", "")
	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "both --cert and --cert-key must be provided together")
}

func TestBuildHTTPClient_KeyWithoutCert(t *testing.T) {
	t.Parallel()

	client, err := buildHTTPClient(false, "", "", "/path/to/key.pem")
	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "both --cert and --cert-key must be provided together")
}

func TestBuildHTTPClient_CertAndKey(t *testing.T) {
	t.Parallel()

	// Create temporary cert and key files
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "client.crt")
	keyPath := filepath.Join(tempDir, "client.key")

	// Generate a self-signed certificate and key for testing
	cert, key := generateTestCertAndKey(t)
	err := os.WriteFile(certPath, cert, 0o600)
	require.NoError(t, err)
	err = os.WriteFile(keyPath, key, 0o600)
	require.NoError(t, err)

	client, err := buildHTTPClient(false, "", certPath, keyPath)
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.NotEqual(t, http.DefaultClient, client)

	// Check that client certificate is set
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	assert.Len(t, transport.TLSClientConfig.Certificates, 1)
}

func TestBuildHTTPClient_CertNotFound(t *testing.T) {
	t.Parallel()

	client, err := buildHTTPClient(false, "", "/nonexistent/cert.crt", "/nonexistent/key.pem")
	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "failed to load client certificate")
}

func TestBuildHTTPClient_InsecureWithCACert(t *testing.T) {
	t.Parallel()

	// Create a temporary CA cert file
	tempDir := t.TempDir()
	caCertPath := filepath.Join(tempDir, "ca.crt")

	// Generate a valid CA certificate
	caCertPEM := generateTestCACert(t)
	err := os.WriteFile(caCertPath, caCertPEM, 0o600)
	require.NoError(t, err)

	// Both insecure and CA cert can be set together
	client, err := buildHTTPClient(true, caCertPath, "", "")
	require.NoError(t, err)
	require.NotNil(t, client)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
	assert.NotNil(t, transport.TLSClientConfig.RootCAs)
}

// generateTestCertAndKey generates a self-signed certificate and key for testing
func generateTestCertAndKey(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	// Generate a new ECDSA private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	// Create a certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Task Org"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	// Create the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	// Encode certificate to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	// Encode private key to PEM
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return certPEM, keyPEM
}

// generateTestCACert generates a self-signed CA certificate for testing
func generateTestCACert(t *testing.T) []byte {
	t.Helper()

	// Generate a new ECDSA private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	// Create a CA certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	// Create the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	// Encode certificate to PEM
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cache.yaml")

	require.NoError(t, writeFileAtomic(path, []byte("version: '3'\n")))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "version: '3'\n", string(got))

	// A second write publishes new contents wholesale, so readers never see a
	// partially rewritten file.
	require.NoError(t, writeFileAtomic(path, []byte("version: '3'\ntasks: {}\n")))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "version: '3'\ntasks: {}\n", string(got))

	// Staging files are removed once the write has been published.
	matches, err := filepath.Glob(filepath.Join(dir, ".task-cache-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)

	// A failed write reports the error without leaving a staging file behind.
	err = writeFileAtomic(filepath.Join(dir, "missing", "cache.yaml"), []byte("x"))
	require.Error(t, err)
	matches, err = filepath.Glob(filepath.Join(dir, "*", ".task-cache-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestCacheNodeCommit(t *testing.T) {
	t.Parallel()

	node, err := NewHTTPNode("https://example.com/dir/Taskfile.yml", "", false)
	require.NoError(t, err)

	cacheDir := t.TempDir()
	cache := NewCacheNode(node, cacheDir)

	data := []byte("version: '3'\n")
	sum := checksum(data)
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, cache.Commit(data, sum, now))

	got, err := cache.Read()
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Equal(t, sum, cache.ReadChecksum())
	assert.Equal(t, now, cache.ReadTimestamp())

	// Re-committing replaces all three artifacts as one unit.
	data2 := []byte("version: '3'\ntasks: {}\n")
	sum2 := checksum(data2)
	require.NoError(t, cache.Commit(data2, sum2, now.Add(time.Minute)))

	got, err = cache.Read()
	require.NoError(t, err)
	assert.Equal(t, data2, got)
	assert.Equal(t, sum2, cache.ReadChecksum())
	assert.Equal(t, now.Add(time.Minute), cache.ReadTimestamp())

	matches, err := filepath.Glob(filepath.Join(cacheDir, remoteCacheDir, ".task-cache-*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "no staging files should remain after commit")
}

// TestRemoteReadCommitBoundary makes sure that an abnormal response status and
// an interrupted body read both fail without committing anything to the cache,
// while a following retry issues a fresh request and commits the complete
// response, which subsequent (including offline) runs then reuse.
func TestRemoteReadCommitBoundary(t *testing.T) {
	const want = "version: '3'\n"

	tests := []struct {
		name  string
		abort func(t *testing.T, w http.ResponseWriter)
	}{
		{
			name: "abnormal status",
			abort: func(_ *testing.T, w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "interrupted read",
			abort: func(t *testing.T, w http.ResponseWriter) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("test server does not support hijacking")
					return
				}
				conn, bufrw, err := hj.Hijack()
				if err != nil {
					t.Errorf("hijack failed: %v", err)
					return
				}
				defer conn.Close()
				// Promise a larger body than is sent, then drop the connection
				// so the client's read is interrupted.
				_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\n" +
					"Content-Type: text/yaml\r\n" +
					"Content-Length: 1000\r\n" +
					"Connection: close\r\n\r\n" +
					"partial")
				_ = bufrw.Flush()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/yaml")
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)
					return
				}
				if gets.Add(1) == 1 {
					tt.abort(t, w)
					return
				}
				_, _ = w.Write([]byte(want))
			}))
			defer srv.Close()

			node, err := NewHTTPNode(srv.URL+"/Taskfile.yml", "", true)
			require.NoError(t, err)

			cacheDir := t.TempDir()
			cache := NewCacheNode(node, cacheDir)
			newReader := func(offline bool) *Reader {
				opts := []ReaderOption{
					WithTempDir(cacheDir),
					WithCacheExpiryDuration(time.Hour),
				}
				if offline {
					opts = append(opts, WithOffline(true))
				}
				return NewReader(opts...)
			}

			// First attempt fails: the error returns without committing
			// anything, so no partial content is observable at the cache path.
			_, err = newReader(false).readRemoteNodeContent(t.Context(), node)
			require.Error(t, err)

			_, err = cache.Read()
			require.ErrorIs(t, err, os.ErrNotExist)
			matches, err := filepath.Glob(filepath.Join(cacheDir, remoteCacheDir, ".task-cache-*"))
			require.NoError(t, err)
			assert.Empty(t, matches)

			// The retry makes a fresh request and only the complete response is
			// committed.
			b, err := newReader(false).readRemoteNodeContent(t.Context(), node)
			require.NoError(t, err)
			assert.Equal(t, want, string(b))
			assert.Equal(t, int32(2), gets.Load())

			cached, err := cache.Read()
			require.NoError(t, err)
			assert.Equal(t, want, string(cached))

			// A fresh cache is served without issuing another request...
			b, err = newReader(false).readRemoteNodeContent(t.Context(), node)
			require.NoError(t, err)
			assert.Equal(t, want, string(b))
			assert.Equal(t, int32(2), gets.Load(), "the committed cache must be reused without re-requesting")

			// ...and the same committed cache backs offline runs.
			b, err = newReader(true).readRemoteNodeContent(t.Context(), node)
			require.NoError(t, err)
			assert.Equal(t, want, string(b))
		})
	}
}
