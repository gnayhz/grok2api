package inference

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Browser TLS validates the production asset hostname. A child test process
// trusts only this test's temporary CA; no system trust, DNS, hosts file or
// production transport option is changed to make local HTTPS pass.
func imageAssetTLSChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv("GROK_IMAGE_TLS_CHILD") == "1" {
		return true
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "image test"}, DNSNames: []string{"assets.grok.com", "auth.x.ai", "vidgen.x.ai"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=100s", "-test.v")
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "SSL_CERT_FILE=") && !strings.HasPrefix(item, "SSL_CERT_DIR=") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "GROK_IMAGE_TLS_CHILD=1", "SSL_CERT_FILE="+certPath, "SSL_CERT_DIR="+dir, "GROK_IMAGE_TLS_KEY="+keyPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated asset TLS tests: %v\n%s", err, output)
	}
	t.Logf("isolated asset TLS results:\n%s", output)
	return false
}

func imageAssetProxy(t *testing.T, primaryURL string, serve func(http.ResponseWriter, *http.Request)) string {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(os.Getenv("SSL_CERT_FILE"), os.Getenv("GROK_IMAGE_TLS_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	asset := httptest.NewUnstartedServer(http.HandlerFunc(serve))
	asset.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	asset.StartTLS()
	t.Cleanup(asset.Close)
	plain := &http.Transport{Proxy: nil}
	t.Cleanup(plain.CloseIdleConnections)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			target := asset.Listener.Addr().String()
			if r.Host == strings.TrimPrefix(primaryURL, "http://") {
				target = r.Host
			} else if r.Host != "assets.grok.com:443" && r.Host != "auth.x.ai:443" && r.Host != "vidgen.x.ai:443" {
				http.Error(w, "unexpected target", 400)
				return
			}
			upstream, err := net.DialTimeout("tcp", target, time.Second)
			if err != nil {
				http.Error(w, "local asset unavailable", 502)
				return
			}
			defer upstream.Close()
			client, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(10 * time.Second))
			_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
			_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buffered.Flush()
			done := make(chan struct{})
			go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close(); close(done) }()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			<-done
			return
		}
		if !strings.HasPrefix(r.URL.String(), primaryURL+"/") {
			http.Error(w, "unexpected plain target", 400)
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		response, err := plain.RoundTrip(request)
		if err != nil {
			http.Error(w, "local primary unavailable", 502)
			return
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusSwitchingProtocols {
			client, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(10 * time.Second))
			_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
			_ = response.Header.Write(buffered)
			_, _ = buffered.WriteString("\r\n")
			_ = buffered.Flush()
			duplex := response.Body.(io.ReadWriteCloser)
			done := make(chan struct{})
			go func() { _, _ = io.Copy(duplex, buffered); _ = duplex.Close(); close(done) }()
			_, _ = io.Copy(client, duplex)
			_ = client.Close()
			<-done
			return
		}
		for k, v := range response.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(imageProxyFlushWriter{w}, response.Body)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

type imageProxyFlushWriter struct{ http.ResponseWriter }

func (w imageProxyFlushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.ResponseWriter.(http.Flusher).Flush()
	return n, err
}
