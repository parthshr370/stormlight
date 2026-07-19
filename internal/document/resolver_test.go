package document

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyResolverHappyPath(t *testing.T) {
	body := buildTestPDF(2)
	resolver, cacheRoot, server := newTLSResolver(t, int64(len(body)+100), func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", got)
		}
		writer.Header().Set("Content-Type", "application/pdf")
		_, _ = writer.Write(body)
	})

	media, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+"/asset", "report.pdf"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	wantDigest := sha256.Sum256(body)
	if media.ID != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("MediaRef.ID = %q, want SHA-256 hex", media.ID)
	}
	if media.Filename != "report.pdf" {
		t.Fatalf("MediaRef.Filename = %q, want report.pdf", media.Filename)
	}
	if media.MediaType != "application/pdf" {
		t.Fatalf("MediaRef.MediaType = %q, want application/pdf", media.MediaType)
	}
	if media.SizeBytes != int64(len(body)) {
		t.Fatalf("MediaRef.SizeBytes = %d, want %d", media.SizeBytes, len(body))
	}
	if media.PageCount != 2 {
		t.Fatalf("MediaRef.PageCount = %d, want 2", media.PageCount)
	}
	if media.SHA256 != wantDigest {
		t.Fatalf("MediaRef.SHA256 = %x, want %x", media.SHA256, wantDigest)
	}
	if media.Blob.Store != SessionNamespace("session") || media.Blob.Key != media.ID {
		t.Fatalf("MediaRef.Blob = %+v, want session-local SHA-256 ref", media.Blob)
	}

	reader := NewCacheRootBlobReader(cacheRoot)
	stored, err := reader.OpenBlob(context.Background(), media.Blob.Store, media.Blob.Key)
	if err != nil {
		t.Fatalf("OpenBlob() error = %v", err)
	}
	defer stored.Close()
	storedBody, err := io.ReadAll(stored)
	if err != nil {
		t.Fatalf("io.ReadAll(stored) error = %v", err)
	}
	if !bytes.Equal(storedBody, body) {
		t.Fatal("stored blob differs from downloaded body")
	}
}

func TestLegacyResolver_ExtractsOOXMLToDerivedTextBlob(t *testing.T) {
	var documentPackage bytes.Buffer
	archive := zip.NewWriter(&documentPackage)
	contentTypes, err := archive.Create("[Content_Types].xml")
	if err != nil {
		t.Fatalf("Create([Content_Types].xml) error = %v", err)
	}
	_, err = io.WriteString(contentTypes, `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)
	if err != nil {
		t.Fatalf("write [Content_Types].xml error = %v", err)
	}
	documentXML, err := archive.Create("word/document.xml")
	if err != nil {
		t.Fatalf("Create(word/document.xml) error = %v", err)
	}
	_, err = io.WriteString(documentXML, `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Quarterly Ledger</w:t></w:r></w:p></w:body></w:document>`)
	if err != nil {
		t.Fatalf("write word/document.xml error = %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("archive.Close() error = %v", err)
	}
	body := documentPackage.Bytes()
	resolver, cacheRoot, server := newTLSResolver(t, int64(len(body)+100), func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", docxMediaType)
		_, _ = writer.Write(body)
	})

	media, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+"/asset", "ledger.docx"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if media.MediaType != docxMediaType {
		t.Fatalf("MediaRef.MediaType = %q, want %q", media.MediaType, docxMediaType)
	}
	if media.Delivery == nil {
		t.Fatal("MediaRef.Delivery = nil, want extracted text delivery")
	}
	if media.Delivery.MediaType != "text/plain" {
		t.Fatalf("MediaRef.Delivery.MediaType = %q, want text/plain", media.Delivery.MediaType)
	}

	reader := NewCacheRootBlobReader(cacheRoot)
	delivered, err := reader.OpenBlob(context.Background(), media.Delivery.Blob.Store, media.Delivery.Blob.Key)
	if err != nil {
		t.Fatalf("OpenBlob(delivery) error = %v", err)
	}
	deliveredText, err := io.ReadAll(delivered)
	closeErr := delivered.Close()
	if err != nil {
		t.Fatalf("io.ReadAll(delivery) error = %v", err)
	}
	if closeErr != nil {
		t.Fatalf("Close(delivery) error = %v", closeErr)
	}
	if !bytes.Contains(deliveredText, []byte("Quarterly Ledger")) {
		t.Fatalf("derived text = %q, want Quarterly Ledger", deliveredText)
	}

	raw, err := reader.OpenBlob(context.Background(), media.Blob.Store, media.Blob.Key)
	if err != nil {
		t.Fatalf("OpenBlob(raw) error = %v", err)
	}
	defer raw.Close()
	rawBytes, err := io.ReadAll(raw)
	if err != nil {
		t.Fatalf("io.ReadAll(raw) error = %v", err)
	}
	if !bytes.Equal(rawBytes, body) {
		t.Fatal("raw blob differs from downloaded document")
	}
}

func TestLegacyResolver_PlainTextHasNoDerivedDelivery(t *testing.T) {
	body := []byte("hello notes")
	resolver, _, server := newTLSResolver(t, int64(len(body)+100), func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write(body)
	})

	media, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+"/asset", "notes.txt"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if media.MediaType != "text/plain" {
		t.Fatalf("MediaRef.MediaType = %q, want text/plain", media.MediaType)
	}
	if media.Delivery != nil {
		t.Fatalf("MediaRef.Delivery = %+v, want nil", media.Delivery)
	}
}

func TestLegacyResolverRejectsDisallowedOrigin(t *testing.T) {
	cacheRoot := newTestCacheRoot(t)
	resolver := NewLegacyResolver(cacheRoot, []string{"assets.example.test"}, 1024, time.Second)

	_, err := resolver.Resolve(context.Background(), "session", legacyAttachment("https://other.example.test/asset", "report.pdf"))
	requireDocumentErrorCode(t, err, CodeOriginNotAllowed)
	requireNoStoredBlob(t, cacheRoot)
}

func TestLegacyResolverRejectsHTTP(t *testing.T) {
	cacheRoot := newTestCacheRoot(t)
	resolver := NewLegacyResolver(cacheRoot, []string{"assets.example.test"}, 1024, time.Second)

	_, err := resolver.Resolve(context.Background(), "session", legacyAttachment("http://assets.example.test/asset", "report.pdf"))
	requireDocumentErrorCode(t, err, CodeOriginNotAllowed)
	requireNoStoredBlob(t, cacheRoot)
}

func TestValidateLegacyURLMatchesAllowedHostAndPort(t *testing.T) {
	tests := []struct {
		name        string
		allowed     string
		requestURL  string
		wantAllowed bool
	}{
		{
			name:        "full URL uses default HTTPS port",
			allowed:     "https://asset.example.com/v3/assets",
			requestURL:  "https://asset.example.com/x",
			wantAllowed: true,
		},
		{
			name:        "bare host uses default HTTPS port",
			allowed:     "asset.example.com",
			requestURL:  "https://asset.example.com/x",
			wantAllowed: true,
		},
		{
			name:        "bare host rejects non-default port",
			allowed:     "asset.example.com",
			requestURL:  "https://asset.example.com:8443/x",
			wantAllowed: false,
		},
		{
			name:        "host and port match explicit port",
			allowed:     "asset.example.com:8443",
			requestURL:  "https://asset.example.com:8443/x",
			wantAllowed: true,
		},
		{
			name:        "explicit port rejects default port",
			allowed:     "asset.example.com:8443",
			requestURL:  "https://asset.example.com/x",
			wantAllowed: false,
		},
		{
			name:        "different host is rejected",
			allowed:     "asset.example.com",
			requestURL:  "https://other.example.com/x",
			wantAllowed: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := NewLegacyResolver("", []string{test.allowed}, 1024, time.Second)
			assetURL, err := url.Parse(test.requestURL)
			if err != nil {
				t.Fatalf("url.Parse() error = %v", err)
			}
			err = validateLegacyURL(assetURL, resolver.allowedOrigins)
			if test.wantAllowed {
				if err != nil {
					t.Fatalf("validateLegacyURL() error = %v", err)
				}
				return
			}
			requireDocumentErrorCode(t, err, CodeOriginNotAllowed)
		})
	}
}

func TestParseAllowedOriginSkipsGarbage(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry string
	}{
		{name: "empty", entry: ""},
		{name: "colons", entry: "::::"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if origin, ok := parseAllowedOrigin(test.entry); ok {
				t.Fatalf("parseAllowedOrigin(%q) = %+v, true; want false", test.entry, origin)
			}
		})
	}

	resolver := NewLegacyResolver("", []string{"", "::::"}, 1024, time.Second)
	if len(resolver.allowedOrigins) != 0 {
		t.Fatalf("allowed origins = %+v, want no parsed origins", resolver.allowedOrigins)
	}
	assetURL, err := url.Parse("https://asset.example.com/x")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	requireDocumentErrorCode(t, validateLegacyURL(assetURL, resolver.allowedOrigins), CodeOriginNotAllowed)
}

func TestLegacyResolverRejectsContentLengthMismatch(t *testing.T) {
	body := buildTestPDF(1)
	cacheRoot := newTestCacheRoot(t)
	resolver := NewLegacyResolver(cacheRoot, []string{"assets.example.test"}, int64(len(body)+100), time.Second)
	resolver.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/pdf"}},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body) + 1),
		}, nil
	})

	_, err := resolver.Resolve(context.Background(), "session", legacyAttachment("https://assets.example.test/asset", "report.pdf"))
	requireDocumentErrorCode(t, err, CodeIntegrityMismatch)
	requireNoStoredBlob(t, cacheRoot)
}

func TestLegacyResolverRejectsOversizedBody(t *testing.T) {
	body := buildTestPDF(1)
	resolver, cacheRoot, server := newTLSResolver(t, int64(len(body)-1), func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/pdf")
		_, _ = writer.Write(body)
	})

	_, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+"/asset", "report.pdf"))
	requireDocumentErrorCode(t, err, CodeRequestSizeExceeded)
	requireNoStoredBlob(t, cacheRoot)
}

func TestLegacyResolverRejectsServerError(t *testing.T) {
	resolver, cacheRoot, server := newTLSResolver(t, 1024, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})

	_, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+"/asset", "report.pdf"))
	requireDocumentErrorCode(t, err, CodeDownloadFailed)
	requireNoStoredBlob(t, cacheRoot)
}

func TestLegacyResolverRejectsCorruptPDF(t *testing.T) {
	body := []byte("%PDF-1.4\ngarbage")
	resolver, cacheRoot, server := newTLSResolver(t, 1024, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/pdf")
		_, _ = writer.Write(body)
	})

	_, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+"/asset", "report.pdf"))
	requireDocumentErrorCode(t, err, CodeCorruptOrEncrypted)
	requireNoStoredBlob(t, cacheRoot)
}

func TestLegacyResolverValidatesRedirectOriginPort(t *testing.T) {
	body := buildTestPDF(1)
	for _, test := range []struct {
		name        string
		path        string
		wantAllowed bool
	}{
		{name: "same host and port", path: "/redirect-same-port", wantAllowed: true},
		{name: "same host different port", path: "/redirect-different-port", wantAllowed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver, cacheRoot, server := newTLSResolver(t, int64(len(body)+100), func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/redirect-same-port":
					http.Redirect(writer, request, "/asset", http.StatusFound)
				case "/redirect-different-port":
					host, port, err := net.SplitHostPort(request.Host)
					if err != nil {
						http.Error(writer, "invalid request host", http.StatusInternalServerError)
						return
					}
					differentPort := "443"
					if port == differentPort {
						differentPort = "444"
					}
					http.Redirect(writer, request, "https://"+net.JoinHostPort(host, differentPort)+"/asset", http.StatusFound)
				case "/asset":
					writer.Header().Set("Content-Type", "application/pdf")
					_, _ = writer.Write(body)
				default:
					http.NotFound(writer, request)
				}
			})

			_, err := resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL+test.path, "report.pdf"))
			if test.wantAllowed {
				if err != nil {
					t.Fatalf("Resolve() error = %v", err)
				}
				return
			}
			requireDocumentErrorCode(t, err, CodeOriginNotAllowed)
			requireNoStoredBlob(t, cacheRoot)
		})
	}
}

func TestLegacyResolverBlocksLoopbackEndToEnd(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(buildTestPDF(1))
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	cacheRoot := newTestCacheRoot(t)
	resolver := NewLegacyResolver(cacheRoot, []string{serverURL.Host}, 4096, time.Second)

	_, err = resolver.Resolve(context.Background(), "session", legacyAttachment(server.URL, "report.pdf"))
	requireDocumentErrorCode(t, err, CodeSSRFBlocked)
	requireNoStoredBlob(t, cacheRoot)
}

func TestValidateDialAddress(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:443",
		"10.0.0.1:443",
		"172.16.0.1:443",
		"192.168.0.1:443",
		"169.254.169.254:80",
		"100.64.0.1:443",
		"0.0.0.0:443",
		"224.0.0.1:443",
		"[::1]:443",
		"[fc00::1]:443",
		"[fe80::1]:443",
		"[ff02::1]:443",
		"[::]:443",
	} {
		t.Run(address, func(t *testing.T) {
			requireDocumentErrorCode(t, validateDialAddress(address), CodeSSRFBlocked)
		})
	}
	if err := validateDialAddress("8.8.8.8:443"); err != nil {
		t.Fatalf("validateDialAddress(public) error = %v", err)
	}
}

func newTLSResolver(t *testing.T, maxBytes int64, handler http.HandlerFunc) (*LegacyResolver, string, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}

	cacheRoot := newTestCacheRoot(t)
	resolver := NewLegacyResolver(cacheRoot, []string{serverURL.Host}, maxBytes, 5*time.Second)
	resolver.dialer = &net.Dialer{}
	transport, ok := resolver.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("resolver transport type = %T, want *http.Transport", resolver.client.Transport)
	}
	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("server transport type = %T, want *http.Transport", server.Client().Transport)
	}
	transport.TLSClientConfig = serverTransport.TLSClientConfig.Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	return resolver, cacheRoot, server
}

func newTestCacheRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func legacyAttachment(reference, filename string) AttachmentInput {
	return AttachmentInput{
		Source: AttachmentSource{
			Kind:      SourceLegacyAssetURL,
			Reference: reference,
		},
		Filename: filename,
	}
}

func requireNoStoredBlob(t *testing.T, cacheRoot string) {
	t.Helper()
	err := filepath.WalkDir(cacheRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			t.Fatalf("unexpected blob file after failed resolution: %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("filepath.WalkDir() error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
