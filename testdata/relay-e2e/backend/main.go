// Echo backends for stacklane-relay Docker E2E.
// Modes via env:
//   MODE=http   — HTTP on :8080 responding "http-ok"
//   MODE=https  — HTTPS (self-signed) on :8443 responding "https-ok"
//   MODE=udp    — UDP echo on :9000
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "http"
	}
	switch mode {
	case "http":
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("http-ok"))
		})
		log.Fatal(http.ListenAndServe(":8080", mux))
	case "https":
		cert, err := selfSigned()
		if err != nil {
			log.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("https-ok"))
		})
		srv := &http.Server{
			Addr:      ":8443",
			Handler:   mux,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		}
		log.Fatal(srv.ListenAndServeTLS("", ""))
	case "udp":
		pc, err := net.ListenPacket("udp", ":9000")
		if err != nil {
			log.Fatal(err)
		}
		defer pc.Close()
		buf := make([]byte, 65535)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				log.Fatal(err)
			}
			// Prefix so multi-client tests can see distinct echoes if needed.
			out := append([]byte("udp:"), buf[:n]...)
			_, _ = pc.WriteTo(out, addr)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown MODE %q\n", mode)
		os.Exit(2)
	}
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay-e2e-backend"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"backend", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}
