package controller

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNewStorageAPIHTTPClientVerifiesTLSByDefault(t *testing.T) {
	client, err := newStorageAPIHTTPClient(TLSConfig{})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS verification is disabled by default")
	}
}

func TestNewStorageAPIHTTPClientCanDisableTLSVerification(t *testing.T) {
	client, err := newStorageAPIHTTPClient(TLSConfig{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS verification was not disabled")
	}
}

func TestNewStorageAPIHTTPClientRejectsInvalidCABundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := newStorageAPIHTTPClient(TLSConfig{CABundlePath: path})
	if err == nil {
		t.Fatal("invalid CA bundle was accepted")
	}
}

func TestNewStorageAPIHTTPClientAddsCABundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte(testCertificate), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newStorageAPIHTTPClient(TLSConfig{CABundlePath: path})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("custom root CA pool was not configured")
	}
}

// A test-only self-signed certificate; no private key is included.
const testCertificate = `-----BEGIN CERTIFICATE-----
MIIDBTCCAe2gAwIBAgIUfbfZTcSA9xV4hZmh48upX9JTKZQwDQYJKoZIhvcNAQEL
BQAwEjEQMA4GA1UEAwwHdGVzdC1jYTAeFw0yNjA3MTcxOTIzNDVaFw0yNjA3MTgx
OTIzNDVaMBIxEDAOBgNVBAMMB3Rlc3QtY2EwggEiMA0GCSqGSIb3DQEBAQUAA4IB
DwAwggEKAoIBAQDZWFGdq6xVaQlf+9uzCVDclS8mGeVeZf6ZsdIYcNj+M8p4q1IM
r7A/KRi6ef8m/JqMI/6h42RoHfcLnLa+UgrleFhsGcDzEyGb/ZnOTF5wT1ZqRvxi
CAcc0qqSNsgQc5Qa3DCkhh4o0BpceY9QZgdG78O3M1HIdf7chS5n3xJdiAOisB4c
660aye7nIW5BxoKmD9F4IzS7loQ2FGX+3yJswrltvycmiHWchxR5CGvlEmMNKfz/
Q05nhkcM8Ziyc9QRATSBi5IMDKb9P9SnFO6d0IS+AlnXhnYoRksS5lgX2Oi6PwQz
nOxCwtFpUntnkRs3wdDg9bFAnp2dnkL9p4AJAgMBAAGjUzBRMB0GA1UdDgQWBBRq
O9ZuTVeVVn6V43O0EEIZz4Fk4DAfBgNVHSMEGDAWgBRqO9ZuTVeVVn6V43O0EEIZ
z4Fk4DAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IBAQA6LN9QLjhk
ihKN+6yvseZQ3JSPPzCawRYt5BgyTwJiqSlHTI/q5y1RtaQRo4Bhl0R9ItESNfJo
GlFD0IBsDB2ucHsc7NxYEHmOuPdv4Th4FmlnvPrWTfpav6Q2YReJT/VpRfYJSUKn
x8luYWwBcC77E8gOb+iqomvJfqGQCH2Rl6eZMDCQai+/UMStopXq1Rc9jw1PgORS
tfBfQ/51shxzxoZt/Nl7i9plXf3TosKTunalZ2+uWF8CdiH0xmE/pMNJIUwCX5D+
D07uHjeSxDkoxGACgmdkQXAB7hvSvVwmE3HcQXnRnWrX34D/+PRR+T6Rp568yIsw
qeAoNzelAsOV
-----END CERTIFICATE-----`
