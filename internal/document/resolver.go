package document

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultHTTPSPort = "443"

// allowedOrigin stores the canonical host and port used for exact allowlist comparisons.
type allowedOrigin struct {
	host string
	port string
}

// LegacyResolver downloads legacy asset URLs into a session-local blob store.
type LegacyResolver struct {
	cacheRoot      string
	client         *http.Client
	allowedOrigins []allowedOrigin
	maxBytes       int64
	dialer         *net.Dialer
}

// NewLegacyResolver returns a resolver that accepts HTTPS URLs from allowedOrigins.
func NewLegacyResolver(cacheRoot string, allowedOrigins []string, maxBytes int64, timeout time.Duration) *LegacyResolver {
	parsedAllowedOrigins := make([]allowedOrigin, 0, len(allowedOrigins))
	for _, entry := range allowedOrigins {
		if origin, ok := parseAllowedOrigin(entry); ok {
			parsedAllowedOrigins = append(parsedAllowedOrigins, origin)
		}
	}
	resolver := &LegacyResolver{
		cacheRoot:      cacheRoot,
		allowedOrigins: parsedAllowedOrigins,
		maxBytes:       maxBytes,
		dialer: &net.Dialer{
			Control: secureDialControl,
		},
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return resolver.dialer.DialContext(ctx, network, address)
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	resolver.client = &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return validateLegacyURL(req.URL, resolver.allowedOrigins)
		},
	}
	return resolver
}

// Resolve downloads, validates, and promotes one legacy attachment.
func (r *LegacyResolver) Resolve(ctx context.Context, sessionID string, in AttachmentInput) (MediaRef, error) {
	if in.Source.Kind != SourceLegacyAssetURL {
		return MediaRef{}, NewDocumentError(
			CodeUnsupportedAttachmentSource,
			"The attachment source is not supported.",
			nil,
			nil,
		)
	}

	assetURL, err := url.Parse(in.Source.Reference)
	if err != nil {
		return MediaRef{}, originNotAllowedError()
	}
	if err := validateLegacyURL(assetURL, r.allowedOrigins); err != nil {
		return MediaRef{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	if err != nil {
		return MediaRef{}, downloadFailedError()
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := r.client.Do(request)
	if err != nil {
		var documentErr *DocumentError
		if errors.As(err, &documentErr) {
			return MediaRef{}, documentErr
		}
		return MediaRef{}, downloadFailedError()
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return MediaRef{}, downloadFailedError()
	}
	if r.cacheRoot == "" {
		return MediaRef{}, downloadFailedError()
	}
	namespace := SessionNamespace(sessionID)
	store, err := newScopedBlobStore(filepath.Join(r.cacheRoot, namespace), namespace)
	if err != nil {
		return MediaRef{}, downloadFailedError()
	}

	temp, err := store.CreateTemp()
	if err != nil {
		return MediaRef{}, downloadFailedError()
	}
	tempPath := temp.Name()
	promoted := false
	defer func() {
		if !promoted {
			_ = temp.Close()
			_ = os.Remove(tempPath)
		}
	}()

	limit := r.maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	if limit < 0 {
		limit = 0
	}
	hasher := sha256.New()
	count, err := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(response.Body, limit))
	if err != nil {
		return MediaRef{}, downloadFailedError()
	}
	if count > r.maxBytes {
		return MediaRef{}, NewDocumentError(
			CodeRequestSizeExceeded,
			"The attachment is too large.",
			nil,
			nil,
		)
	}
	if response.ContentLength >= 0 && response.ContentLength != count {
		return MediaRef{}, NewDocumentError(
			CodeIntegrityMismatch,
			"The attachment size did not match the download metadata.",
			nil,
			nil,
		)
	}
	if err := temp.Close(); err != nil {
		return MediaRef{}, downloadFailedError()
	}

	mediaType, pageCount, err := Inspect(tempPath, DocumentHints{
		Filename:          in.Filename,
		DeclaredMediaType: resolverDeclaredMediaType(in.Filename, response.Header.Get("Content-Type")),
	})
	if err != nil {
		return MediaRef{}, err
	}

	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))

	if isOOXMLMediaType(mediaType) {
		extracted, extractErr := ExtractOOXMLText(tempPath, mediaType)
		if extractErr != nil {
			return MediaRef{}, extractErr
		}

		textTemp, textErr := store.CreateTemp()
		if textErr != nil {
			return MediaRef{}, downloadFailedError()
		}
		textTempPath := textTemp.Name()
		textPromoted := false
		defer func() {
			if !textPromoted {
				_ = textTemp.Close()
				_ = os.Remove(textTempPath)
			}
		}()

		textHasher := sha256.New()
		if _, writeErr := io.Copy(io.MultiWriter(textTemp, textHasher), strings.NewReader(extracted)); writeErr != nil {
			_ = textTemp.Close()
			return MediaRef{}, downloadFailedError()
		}
		if closeErr := textTemp.Close(); closeErr != nil {
			return MediaRef{}, downloadFailedError()
		}
		var textDigest [sha256.Size]byte
		copy(textDigest[:], textHasher.Sum(nil))

		rawBlob, promoteErr := store.Promote(tempPath, digest)
		if promoteErr != nil {
			return MediaRef{}, downloadFailedError()
		}
		promoted = true
		textBlob, textPromoteErr := store.Promote(textTempPath, textDigest)
		if textPromoteErr != nil {
			return MediaRef{}, downloadFailedError()
		}
		textPromoted = true
		delivery := &DeliveredContent{
			MediaType: "text/plain",
			Blob:      textBlob,
			SizeBytes: int64(len(extracted)),
		}
		id := hex.EncodeToString(digest[:])
		return MediaRef{
			ID:        id,
			Filename:  in.Filename,
			MediaType: mediaType,
			SizeBytes: count,
			PageCount: pageCount,
			SHA256:    digest,
			Blob:      rawBlob,
			Delivery:  delivery,
		}, nil
	}

	blob, err := store.Promote(tempPath, digest)
	if err != nil {
		return MediaRef{}, downloadFailedError()
	}
	promoted = true
	id := hex.EncodeToString(digest[:])
	return MediaRef{
		ID:        id,
		Filename:  in.Filename,
		MediaType: mediaType,
		SizeBytes: count,
		PageCount: pageCount,
		SHA256:    digest,
		Blob:      blob,
	}, nil
}

// validateLegacyURL keeps the initial request and every redirect pinned to an allowlisted HTTPS origin.
func validateLegacyURL(assetURL *url.URL, allowedOrigins []allowedOrigin) error {
	if assetURL == nil || !strings.EqualFold(assetURL.Scheme, "https") || assetURL.Hostname() == "" || assetURL.User != nil {
		return originNotAllowedError()
	}
	port := assetURL.Port()
	if port == "" {
		port = defaultHTTPSPort
	}
	requestOrigin, ok := newAllowedOrigin(assetURL.Hostname(), port)
	if !ok {
		return originNotAllowedError()
	}
	for _, allowedOrigin := range allowedOrigins {
		if requestOrigin == allowedOrigin {
			return nil
		}
	}
	return originNotAllowedError()
}

func parseAllowedOrigin(entry string) (allowedOrigin, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return allowedOrigin{}, false
	}
	if parsed, err := netip.ParseAddr(strings.Trim(entry, "[]")); err == nil {
		return newAllowedOrigin(parsed.String(), defaultHTTPSPort)
	}
	if host, port, err := net.SplitHostPort(entry); err == nil {
		if origin, ok := newAllowedOrigin(host, port); ok {
			return origin, true
		}
	}
	if parsed, err := url.Parse(entry); err == nil && parsed.IsAbs() {
		if !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" || parsed.User != nil {
			return allowedOrigin{}, false
		}
		port := parsed.Port()
		if port == "" {
			port = defaultHTTPSPort
		}
		return newAllowedOrigin(parsed.Hostname(), port)
	}

	parsed, err := url.Parse("https://" + entry)
	if err != nil ||
		parsed.Hostname() == "" ||
		parsed.User != nil ||
		parsed.Port() != "" ||
		parsed.Path != "" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return allowedOrigin{}, false
	}
	return newAllowedOrigin(parsed.Hostname(), defaultHTTPSPort)
}

func newAllowedOrigin(host, port string) (allowedOrigin, bool) {
	host = canonicalHost(host)
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if host == "" || err != nil || portNumber == 0 {
		return allowedOrigin{}, false
	}
	return allowedOrigin{
		host: host,
		port: strconv.FormatUint(portNumber, 10),
	}, true
}

func canonicalHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

var (
	metadataServiceIP       = netip.MustParseAddr("169.254.169.254")
	carrierGradeNATIPPrefix = netip.MustParsePrefix("100.64.0.0/10")
)

// secureDialControl revalidates the resolved socket address so DNS rebinding can't reach internal services.
func secureDialControl(_ string, address string, _ syscall.RawConn) error {
	return validateDialAddress(address)
}

// validateDialAddress rejects non-public resolved addresses after the dialer has resolved the hostname.
func validateDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ssrfBlockedError()
	}
	addressIP, err := netip.ParseAddr(host)
	if err != nil {
		return ssrfBlockedError()
	}
	addressIP = addressIP.Unmap()
	if addressIP == metadataServiceIP ||
		carrierGradeNATIPPrefix.Contains(addressIP) ||
		addressIP.IsLoopback() ||
		addressIP.IsPrivate() ||
		addressIP.IsLinkLocalUnicast() ||
		addressIP.IsLinkLocalMulticast() ||
		addressIP.IsMulticast() ||
		addressIP.IsUnspecified() {
		return ssrfBlockedError()
	}
	return nil
}

// resolverDeclaredMediaType gives a known filename extension precedence over an untrusted response header.
func resolverDeclaredMediaType(filename, responseMediaType string) string {
	fromExtension := normalizeMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))))
	if fromExtension != "" && fromExtension != "application/octet-stream" {
		return fromExtension
	}
	if fromResponse := normalizeMediaType(responseMediaType); fromResponse != "" {
		return fromResponse
	}
	return fromExtension
}

func originNotAllowedError() *DocumentError {
	return NewDocumentError(
		CodeOriginNotAllowed,
		"The attachment origin is not allowed.",
		nil,
		nil,
	)
}

func ssrfBlockedError() *DocumentError {
	return NewDocumentError(
		CodeSSRFBlocked,
		"The attachment address is blocked.",
		nil,
		nil,
	)
}

func downloadFailedError() *DocumentError {
	return NewDocumentError(
		CodeDownloadFailed,
		"The attachment could not be downloaded.",
		nil,
		nil,
	)
}
