// Package googleauth loads the Google credentials used by Ghost Wispr.
package googleauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/credentials/idtoken"
	"cloud.google.com/go/auth/credentials/impersonate"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// CloudPlatformScope is required to mint an ID token through IAM Credentials.
const CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// sdkLogger is the logger every credential this package builds gives
// Google's SDK: it discards. With GOOGLE_SDK_GO_LOGGING_LEVEL=debug, the
// SDK's own logger writes each token response whole, the token in it, to
// stderr.
var sdkLogger = slog.New(slog.DiscardHandler)

type externalAccountFile struct {
	Type                           string `json:"type"`
	ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
	CredentialSource               struct {
		Certificate *struct {
			CertificateConfigLocation string `json:"certificate_config_location"`
		} `json:"certificate"`
	} `json:"credential_source"`
}

type certificateConfig struct {
	CertConfigs struct {
		Workload struct {
			CertPath string `json:"cert_path"`
		} `json:"workload"`
	} `json:"cert_configs"`
}

// Detect returns credentials from credPath when it is set, otherwise from
// Application Default Credentials, with tokens scoped for scopes.
func Detect(_ context.Context, credPath string, scopes ...string) (*auth.Credentials, error) {
	options := &credentials.DetectOptions{Scopes: scopes, Logger: sdkLogger}
	credPath = strings.TrimSpace(credPath)
	if credPath == "" {
		return credentials.DetectDefault(options)
	}

	data, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("read Google credentials: %w", err)
	}
	var config externalAccountFile
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse Google credentials: %w", err)
	}
	switch config.Type {
	case string(credentials.ServiceAccount):
		return credentials.NewCredentialsFromFile(credentials.ServiceAccount, credPath, options)
	case string(credentials.ExternalAccount):
		return credentials.NewCredentialsFromFile(credentials.ExternalAccount, credPath, options)
	default:
		return nil, fmt.Errorf("unsupported Google credential type %q", config.Type)
	}
}

// setCredentialsFile is what an operator sets when the unit's Google
// credentials cannot mint an ID token.
const setCredentialsFile = "set google_credentials_file to the unit's workload identity credential file"

// credentialJSON is the credential file at credPath, or the one Application
// Default Credentials found; nil when they found none, as on a Compute
// Engine VM, whose metadata server is its credential.
func credentialJSON(ctx context.Context, credPath string) ([]byte, error) {
	credPath = strings.TrimSpace(credPath)
	if credPath != "" {
		data, err := os.ReadFile(credPath)
		if err != nil {
			return nil, fmt.Errorf("read Google credentials: %w", err)
		}
		return data, nil
	}

	credentials, err := Detect(ctx, "", CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("detect Google credentials (%s): %w", setCredentialsFile, err)
	}
	if data := credentials.JSON(); len(data) != 0 {
		return data, nil
	}
	return nil, nil
}

// IDToken mints an ID token for audience from the Google credentials at
// credPath, or from Application Default Credentials without one: an
// external-account file (workload identity federation) by impersonating the
// service account it names, a service-account key file by signing a request
// for one, and on a Compute Engine VM with no file, its own service
// account's from the metadata server.
func IDToken(ctx context.Context, credPath, audience string) (string, error) {
	credentialData, err := credentialJSON(ctx, credPath)
	if err != nil {
		return "", err
	}
	if credentialData == nil {
		// The full format carries the google.compute_engine claims a
		// gateway account pinned to its VM is matched on.
		creds, err := idtoken.NewCredentials(&idtoken.Options{Audience: audience, ComputeTokenFormat: idtoken.ComputeTokenFormatFull, Logger: sdkLogger})
		if err != nil {
			return "", fmt.Errorf("create ID token credentials from the metadata server: %w", err)
		}
		return mintIDToken(ctx, creds)
	}

	var config externalAccountFile
	if err := json.Unmarshal(credentialData, &config); err != nil {
		return "", fmt.Errorf("parse Google credentials: %w", err)
	}
	switch config.Type {
	case string(credentials.ExternalAccount):
	case string(credentials.ServiceAccount):
		creds, err := idtoken.NewCredentialsFromJSON(credentials.ServiceAccount, credentialData, &idtoken.Options{Audience: audience, Logger: sdkLogger})
		if err != nil {
			return "", fmt.Errorf("create ID token credentials from the service-account key: %w", err)
		}
		return mintIDToken(ctx, creds)
	default:
		return "", fmt.Errorf("google credentials of type %q cannot mint an ID token: %s", config.Type, setCredentialsFile)
	}
	principal, err := serviceAccountFromImpersonationURL(config.ServiceAccountImpersonationURL)
	if err != nil {
		return "", err
	}

	var baseConfig map[string]json.RawMessage
	if err := json.Unmarshal(credentialData, &baseConfig); err != nil {
		return "", fmt.Errorf("parse Google credentials: %w", err)
	}
	delete(baseConfig, "service_account_impersonation_url")
	baseCredentialData, err := json.Marshal(baseConfig)
	if err != nil {
		return "", fmt.Errorf("prepare federated credentials: %w", err)
	}
	baseCredentials, err := credentials.NewCredentialsFromJSON(
		credentials.ExternalAccount,
		baseCredentialData,
		&credentials.DetectOptions{Scopes: []string{CloudPlatformScope}, Logger: sdkLogger},
	)
	if err != nil {
		return "", fmt.Errorf("detect federated credentials: %w", err)
	}
	idTokenCredentials, err := impersonate.NewIDTokenCredentials(&impersonate.IDTokenOptions{
		Audience:        audience,
		TargetPrincipal: principal,
		IncludeEmail:    true,
		Credentials:     baseCredentials,
		Logger:          sdkLogger,
	})
	if err != nil {
		return "", fmt.Errorf("create ID token credentials: %w", err)
	}
	return mintIDToken(ctx, idTokenCredentials)
}

// mintIDToken gets the ID token creds hold. Its error keeps only the status
// and code of Google's answer (impersonationError).
func mintIDToken(ctx context.Context, creds *auth.Credentials) (string, error) {
	token, err := creds.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("mint ID token: %w", impersonationError(err))
	}
	return token.Value, nil
}

// impersonationError is err, the impersonation's failure, reduced: IAM's
// answer, which the impersonation quotes ("impersonate: status code 403:
// <answer>"), can hold what it was sent, so only the status that prefix
// names is kept (providererr).
func impersonationError(err error) error {
	reduced := providererr.From("google", err)
	var answered *providererr.Error
	if errors.As(reduced, &answered) && answered.Withheld && answered.Status == 0 {
		if rest, ok := strings.CutPrefix(err.Error(), "impersonate: status code "); ok && len(rest) > 3 && rest[3] == ':' {
			if status, convErr := strconv.Atoi(rest[:3]); convErr == nil {
				answered.Status = status
			}
		}
	}
	return reduced
}

func serviceAccountFromImpersonationURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse service-account impersonation URL: %w", err)
	}
	const prefix = "/serviceAccounts/"
	const suffix = ":generateAccessToken"
	index := strings.Index(parsed.Path, prefix)
	if index == -1 {
		return "", fmt.Errorf("service-account impersonation URL names no service account")
	}
	principal := strings.TrimSuffix(parsed.Path[index+len(prefix):], suffix)
	if principal == "" || principal == parsed.Path[index+len(prefix):] {
		return "", fmt.Errorf("service-account impersonation URL does not generate an access token")
	}
	return principal, nil
}

// CertExpiry returns the expiry of the X.509 certificate configured by credPath.
// Credentials without an X.509 certificate return ok=false.
func CertExpiry(credPath string) (notAfter time.Time, ok bool, err error) {
	credentialData, err := os.ReadFile(credPath)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read Google credentials: %w", err)
	}

	var credential externalAccountFile
	if err := json.Unmarshal(credentialData, &credential); err != nil {
		return time.Time{}, false, fmt.Errorf("parse Google credentials: %w", err)
	}
	if credential.Type != "external_account" || credential.CredentialSource.Certificate == nil {
		return time.Time{}, false, nil
	}

	certificateConfigPath := credential.CredentialSource.Certificate.CertificateConfigLocation
	if certificateConfigPath == "" {
		return time.Time{}, false, fmt.Errorf("external account certificate config location is empty")
	}
	certificateConfigData, err := os.ReadFile(certificateConfigPath)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read certificate config: %w", err)
	}
	var config certificateConfig
	if err := json.Unmarshal(certificateConfigData, &config); err != nil {
		return time.Time{}, false, fmt.Errorf("parse certificate config: %w", err)
	}
	if config.CertConfigs.Workload.CertPath == "" {
		return time.Time{}, false, fmt.Errorf("certificate config workload cert path is empty")
	}

	certificateData, err := os.ReadFile(config.CertConfigs.Workload.CertPath)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read certificate: %w", err)
	}
	block, _ := pem.Decode(certificateData)
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, false, fmt.Errorf("parse certificate: PEM certificate not found")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse certificate: %w", err)
	}
	return certificate.NotAfter, true, nil
}

// CertificateExpiryWarning returns the status warning for certificates expiring
// within 30 days. The warning names where the renewed certificate is published
// and the renewal instructions.
func CertificateExpiryWarning(now, notAfter time.Time, hasCertificate bool) string {
	if !hasCertificate || notAfter.After(now.AddDate(0, 0, 30)) {
		return ""
	}
	if !notAfter.After(now) {
		return fmt.Sprintf("Google credential certificate expired on %s; %s", notAfter.UTC().Format("2006-01-02"), CertificateRenewal)
	}
	return fmt.Sprintf("Google credential certificate expires on %s; %s", notAfter.UTC().Format("2006-01-02"), CertificateRenewal)
}

// CertificateRenewal tells the operator how to renew the device certificate.
const CertificateRenewal = `renew it with the runbook in docs/pi-setup.md, "Renew the X.509 certificate".`
