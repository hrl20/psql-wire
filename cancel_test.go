package wire

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/lib/pq/oid"
)

type testServer struct {
	server   *Server
	sessions map[int32]*testSession
	mutex    sync.RWMutex
}

type testSession struct {
	ProcessID int32
	SecretKey int32
	Cancel    context.CancelFunc
	Addr      net.Addr
}

func newTestServer(tlsConfig *tls.Config) (*testServer, error) {
	ts := &testServer{sessions: make(map[int32]*testSession)}

	server, err := NewServer(ts.handler, BackendKeyData(ts.backendKeyData),
		CancelRequest(ts.cancelRequest), TLSConfig(tlsConfig))
	if err != nil {
		return nil, err
	}
	ts.server = server
	return ts, nil
}

func (ts *testServer) backendKeyData(ctx context.Context) (int32, int32) {
	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	processID, secretKey := rng.Int31(), rng.Int31()

	ts.mutex.Lock()
	ts.sessions[processID] = &testSession{
		ProcessID: processID,
		SecretKey: secretKey,
		Addr:      RemoteAddress(ctx),
	}
	ts.mutex.Unlock()

	return processID, secretKey
}

func (ts *testServer) cancelRequest(ctx context.Context, processID, secretKey int32) error {
	ts.mutex.RLock()
	session, exists := ts.sessions[processID]
	ts.mutex.RUnlock()

	if !exists || session.SecretKey != secretKey {
		return nil
	}

	if session.Cancel != nil {
		session.Cancel()
	}
	return nil
}

func (ts *testServer) handler(ctx context.Context, query string) (PreparedStatements, error) {
	handle := func(ctx context.Context, writer DataWriter, parameters []Parameter) error {
		queryCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		// Store cancel function in session
		addr := RemoteAddress(ctx)
		ts.mutex.Lock()
		for _, session := range ts.sessions {
			if session.Addr == addr {
				session.Cancel = cancel
				break
			}
		}
		ts.mutex.Unlock()

		// Simulate long operation
		for i := range 3 {
			select {
			case <-queryCtx.Done():
				return queryCtx.Err()
			case <-time.After(2 * time.Second):
				writer.Row([]any{i})
			}
		}

		return writer.Complete("SELECT 3")
	}

	cols := Columns{
		Column{Name: "1", Oid: oid.T_int4, Width: 4},
	}

	return Prepared(NewStatement(handle, WithColumns(cols))), nil
}

func generateTestCert() (tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Test"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	return tls.X509KeyPair(certPEM, keyPEM)
}

func startTestServer(t *testing.T, withTLS bool) {
	var tlsConfig *tls.Config
	if withTLS {
		cert, err := generateTestCert()
		if err != nil {
			t.Fatal(err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	server, err := newTestServer(tlsConfig)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:54321")
	if err != nil {
		t.Fatal(err)
	}
	go server.server.Serve(listener)

}

func testCancellation(t *testing.T, sslMode string) {
	connStr := fmt.Sprintf("host=127.0.0.1 port=54321 dbname=test user=test sslmode=%s", sslMode)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SELECT 1")

	// Check for immediate error
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "canceled") {
			return // Success - got cancellation error
		}
		t.Errorf("Unexpected error: %v", err)
		return
	}

	if rows == nil {
		t.Error("Expected rows but got nil")
		return
	}
	defer rows.Close()

	// Try to iterate through rows - expect to see cancellation here
	rowCount := 0
	for rows.Next() {
		var val int
		err := rows.Scan(&val)
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "canceled") {
				return // Success - got cancellation error
			}
			t.Errorf("Unexpected scan error: %v", err)
			return
		}
		rowCount++
	}

	t.Errorf("Expected cancellation error but query succeeded with %d rows", rowCount)
}

func TestCancellationWithoutTLS(t *testing.T) {
	startTestServer(t, false)
	testCancellation(t, "disable")
}

func TestCancellationWithTLS(t *testing.T) {
	startTestServer(t, true)
	testCancellation(t, "require")
}
