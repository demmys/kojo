package agent

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// Maximum image dimensions to prevent decompression bombs.
const maxImagePixels = 25_000_000 // ~5000x5000

// OTPEntry represents a parsed OTP entry from a QR code or migration payload.
type OTPEntry struct {
	Label     string `json:"label"`
	Issuer    string `json:"issuer"`
	Username  string `json:"username"`
	Secret    string `json:"totpSecret"`
	Algorithm string `json:"algorithm,omitempty"`
	Digits    int    `json:"digits,omitempty"`
	Period    int    `json:"period,omitempty"`
}

// NormalizeTOTPSecret normalizes and validates a TOTP secret.
// Returns the canonical form (upper-case, no padding) or an error.
func NormalizeTOTPSecret(secret string) (string, error) {
	secret = strings.TrimRight(strings.ToUpper(strings.TrimSpace(secret)), "=")
	if secret == "" {
		return "", fmt.Errorf("TOTP secret is empty")
	}
	_, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("invalid TOTP secret (not valid base32): %w", err)
	}
	_, err = totp.GenerateCode(secret, time.Now())
	if err != nil {
		return "", fmt.Errorf("TOTP secret failed code generation test: %w", err)
	}
	return secret, nil
}

// ValidateTOTPParams validates TOTP parameters and normalizes the secret.
// Returns the normalized secret or an error.
func ValidateTOTPParams(secret, algorithm string, digits, period int) (string, error) {
	normalized, err := NormalizeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	switch strings.ToUpper(algorithm) {
	case "", "SHA1", "SHA256", "SHA512":
		// ok
	default:
		return "", fmt.Errorf("unsupported TOTP algorithm: %s (must be SHA1, SHA256, or SHA512)", algorithm)
	}
	if digits != 0 && digits != 6 && digits != 8 {
		return "", fmt.Errorf("unsupported TOTP digits: %d (must be 6 or 8)", digits)
	}
	if period < 0 {
		return "", fmt.Errorf("invalid TOTP period: %d", period)
	}
	return normalized, nil
}

// GenerateTOTPCode generates a current TOTP code for the given secret and parameters.
func GenerateTOTPCode(secret, algorithm string, digits, period int) (string, int64, error) {
	now := time.Now()

	opts := totp.ValidateOpts{
		Algorithm: otp.AlgorithmSHA1,
		Digits:    otp.DigitsSix,
		Period:    30,
	}
	switch strings.ToUpper(algorithm) {
	case "SHA256":
		opts.Algorithm = otp.AlgorithmSHA256
	case "SHA512":
		opts.Algorithm = otp.AlgorithmSHA512
	}
	if digits == 8 {
		opts.Digits = otp.DigitsEight
	}
	if period > 0 {
		opts.Period = uint(period)
	}

	code, err := totp.GenerateCodeCustom(secret, now, opts)
	if err != nil {
		return "", 0, fmt.Errorf("failed to generate TOTP code: %w", err)
	}
	p := int64(opts.Period)
	remaining := p - (now.Unix() % p)
	return code, remaining, nil
}

// DecodeQRImage decodes a QR code from an image reader and returns parsed OTP entries.
func DecodeQRImage(r io.Reader) ([]*OTPEntry, error) {
	// Buffer the input so we can read it twice (DecodeConfig + Decode).
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, 10<<20)); err != nil {
		return nil, fmt.Errorf("failed to read image: %w", err)
	}

	// Check dimensions before full decode to prevent decompression bombs.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image config: %w", err)
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if pixels > maxImagePixels {
		return nil, fmt.Errorf("image too large: %dx%d (%d pixels, max %d)", cfg.Width, cfg.Height, pixels, maxImagePixels)
	}

	img, _, err := image.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return nil, fmt.Errorf("failed to create bitmap: %w", err)
	}

	reader := qrcode.NewQRCodeReader()
	result, err := reader.Decode(bmp, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decode QR code: %w", err)
	}

	uri := result.GetText()
	return ParseOTPURI(uri)
}

// ParseOTPURI parses an otpauth:// or otpauth-migration:// URI and returns OTP entries.
func ParseOTPURI(uri string) ([]*OTPEntry, error) {
	uri = strings.TrimSpace(uri)
	if strings.HasPrefix(uri, "otpauth-migration://") {
		return parseOTPMigration(uri)
	}
	if strings.HasPrefix(uri, "otpauth://totp/") {
		entry, err := parseOTPAuthURI(uri)
		if err != nil {
			return nil, err
		}
		return []*OTPEntry{entry}, nil
	}
	if strings.HasPrefix(uri, "otpauth://hotp/") {
		return nil, fmt.Errorf("HOTP is not supported, only TOTP")
	}
	return nil, fmt.Errorf("unsupported URI scheme: %s", uri)
}

// parseOTPAuthURI parses a standard otpauth://totp/... URI.
func parseOTPAuthURI(uri string) (*OTPEntry, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid otpauth URI: %w", err)
	}

	label := strings.TrimPrefix(u.Path, "/")
	label, _ = url.PathUnescape(label)

	params := u.Query()
	secret := params.Get("secret")
	if secret == "" {
		return nil, fmt.Errorf("missing secret in otpauth URI")
	}

	issuer := params.Get("issuer")
	username := label
	if idx := strings.Index(label, ":"); idx >= 0 {
		if issuer == "" {
			issuer = label[:idx]
		}
		username = strings.TrimSpace(label[idx+1:])
	}

	entry := &OTPEntry{
		Label:    label,
		Issuer:   issuer,
		Username: username,
		Secret:   strings.ToUpper(secret),
	}

	if alg := params.Get("algorithm"); alg != "" {
		entry.Algorithm = strings.ToUpper(alg)
	}
	if d := params.Get("digits"); d != "" {
		fmt.Sscanf(d, "%d", &entry.Digits)
	}
	if p := params.Get("period"); p != "" {
		fmt.Sscanf(p, "%d", &entry.Period)
	}

	return entry, nil
}

// parseOTPMigration parses a Google Authenticator migration URI.
func parseOTPMigration(uri string) ([]*OTPEntry, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid migration URI: %w", err)
	}

	dataStr := u.Query().Get("data")
	if dataStr == "" {
		return nil, fmt.Errorf("missing data parameter in migration URI")
	}

	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(dataStr)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(dataStr)
			if err != nil {
				return nil, fmt.Errorf("failed to decode base64 data: %w", err)
			}
		}
	}

	return decodeMigrationPayload(data)
}

// decodeMigrationPayload decodes the protobuf MigrationPayload.
//
// Wire format:
//
//	MigrationPayload { repeated OtpParameters otp_parameters = 1; }
//	OtpParameters {
//	  bytes secret = 1; string name = 2; string issuer = 3;
//	  int32 algorithm = 4; int32 digits = 5; int32 type = 6;
//	}
func decodeMigrationPayload(data []byte) ([]*OTPEntry, error) {
	var entries []*OTPEntry
	r := bytes.NewReader(data)

	for r.Len() > 0 {
		tag, wtype, err := readProtoTag(r)
		if err != nil {
			return nil, fmt.Errorf("failed to read tag in MigrationPayload: %w", err)
		}
		if tag == 1 && wtype == 2 {
			msgData, err := readProtoBytes(r)
			if err != nil {
				return nil, fmt.Errorf("failed to read otp_parameters: %w", err)
			}
			entry, err := decodeOtpParameters(msgData)
			if err != nil {
				return nil, fmt.Errorf("failed to decode otp_parameters: %w", err)
			}
			// Skip HOTP entries
			if entry != nil {
				entries = append(entries, entry)
			}
		} else {
			if err := skipProtoField(r, wtype); err != nil {
				return nil, fmt.Errorf("failed to skip field %d in MigrationPayload: %w", tag, err)
			}
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no TOTP entries found in migration data")
	}
	return entries, nil
}

func decodeOtpParameters(data []byte) (*OTPEntry, error) {
	entry := &OTPEntry{Digits: 6, Period: 30}
	r := bytes.NewReader(data)
	isHOTP := false

	for r.Len() > 0 {
		tag, wtype, err := readProtoTag(r)
		if err != nil {
			return nil, fmt.Errorf("failed to read tag in OtpParameters: %w", err)
		}
		switch tag {
		case 1: // secret (bytes)
			if wtype != 2 {
				return nil, fmt.Errorf("unexpected wire type %d for secret", wtype)
			}
			secret, err := readProtoBytes(r)
			if err != nil {
				return nil, err
			}
			entry.Secret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
		case 2: // name (string)
			if wtype != 2 {
				return nil, fmt.Errorf("unexpected wire type %d for name", wtype)
			}
			name, err := readProtoBytes(r)
			if err != nil {
				return nil, err
			}
			entry.Label = string(name)
			if idx := strings.Index(entry.Label, ":"); idx >= 0 {
				entry.Username = strings.TrimSpace(entry.Label[idx+1:])
			} else {
				entry.Username = entry.Label
			}
		case 3: // issuer (string)
			if wtype != 2 {
				return nil, fmt.Errorf("unexpected wire type %d for issuer", wtype)
			}
			issuer, err := readProtoBytes(r)
			if err != nil {
				return nil, err
			}
			entry.Issuer = string(issuer)
		case 4: // algorithm (int32): 0=unspecified, 1=SHA1, 2=SHA256, 3=SHA512
			if wtype != 0 {
				return nil, fmt.Errorf("unexpected wire type %d for algorithm", wtype)
			}
			v, err := readProtoVarint(r)
			if err != nil {
				return nil, err
			}
			switch v {
			case 1:
				entry.Algorithm = "SHA1"
			case 2:
				entry.Algorithm = "SHA256"
			case 3:
				entry.Algorithm = "SHA512"
			}
		case 5: // digits (int32): 0=unspecified, 1=six, 2=eight
			if wtype != 0 {
				return nil, fmt.Errorf("unexpected wire type %d for digits", wtype)
			}
			v, err := readProtoVarint(r)
			if err != nil {
				return nil, err
			}
			switch v {
			case 1:
				entry.Digits = 6
			case 2:
				entry.Digits = 8
			}
		case 6: // type (int32): 0=unspecified, 1=HOTP, 2=TOTP
			if wtype != 0 {
				return nil, fmt.Errorf("unexpected wire type %d for type", wtype)
			}
			v, err := readProtoVarint(r)
			if err != nil {
				return nil, err
			}
			if v == 1 {
				isHOTP = true
			}
		default:
			if err := skipProtoField(r, wtype); err != nil {
				return nil, fmt.Errorf("failed to skip field %d in OtpParameters: %w", tag, err)
			}
		}
	}

	if isHOTP {
		return nil, nil // Skip HOTP entries
	}

	if entry.Secret == "" {
		return nil, fmt.Errorf("missing secret in OTP parameters")
	}
	if entry.Issuer != "" && entry.Label == "" {
		entry.Label = entry.Issuer
	}

	return entry, nil
}

// Protobuf wire format helpers

func readProtoTag(r *bytes.Reader) (uint64, uint64, error) {
	v, err := readProtoVarint(r)
	if err != nil {
		return 0, 0, err
	}
	return v >> 3, v & 0x7, nil
}

func readProtoVarint(r *bytes.Reader) (uint64, error) {
	return binary.ReadUvarint(r)
}

func readProtoBytes(r *bytes.Reader) ([]byte, error) {
	length, err := readProtoVarint(r)
	if err != nil {
		return nil, err
	}
	if length > uint64(r.Len()) {
		return nil, fmt.Errorf("protobuf: length %d exceeds remaining %d", length, r.Len())
	}
	buf := make([]byte, length)
	_, err = io.ReadFull(r, buf)
	return buf, err
}

func skipProtoField(r *bytes.Reader, wtype uint64) error {
	switch wtype {
	case 0: // varint
		_, err := readProtoVarint(r)
		return err
	case 1: // 64-bit
		_, err := r.Seek(8, io.SeekCurrent)
		return err
	case 2: // length-delimited
		_, err := readProtoBytes(r)
		return err
	case 5: // 32-bit
		_, err := r.Seek(4, io.SeekCurrent)
		return err
	default:
		return fmt.Errorf("unknown protobuf wire type: %d", wtype)
	}
}
