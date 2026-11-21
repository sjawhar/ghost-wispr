// Package googleauth loads the Google credentials used by Ghost Wispr.
package googleauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
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

// ModelGatewayAudience is the audience of the ID token that the model gateway
// (config's model_gateway_url) signs a caller in by.
const ModelGatewayAudience = "model-gateway-gce"

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
	options := &credentials.DetectOptions{Scopes: scopes}
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

func externalAccountCredentialJSON(ctx context.Context, credPath string) ([]byte, error) {
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
		return nil, fmt.Errorf("detect Google credentials: %w", err)
	}
	if data := credentials.JSON(); len(data) != 0 {
		return data, nil
	}
	return nil, fmt.Errorf("default Google credentials name no external-account file")
}

// IDToken mints an ID token for audience by impersonating the service account
// named in the external-account credential file at credPath.
func IDToken(ctx context.Context, credPath, audience string) (string, error) {
	idTokenCredentials, err := impersonatedIDTokenCredentials(ctx, credPath, audience)
	if err != nil {
		return "", err
	}
	token, err := idTokenCredentials.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("mint ID token: %w", impersonationError(err))
	}
	return token.Value, nil
}

// GatewayCredentials give ID tokens for audience that the model gateway signs
// a caller in by: tokens carrying the account's email, kept until a few
// minutes before they expire. With a credential file at credPath they
// impersonate the service account that external-account file names, as
// IDToken does; with none, they are the metadata server's ID token in full
// format, the developer box's own account.
func GatewayCredentials(ctx context.Context, credPath, audience string) (*auth.Credentials, error) {
	if strings.TrimSpace(credPath) != "" {
		return impersonatedIDTokenCredentials(ctx, credPath, audience)
	}
	creds, err := idtoken.NewCredentials(&idtoken.Options{Audience: audience, ComputeTokenFormat: idtoken.ComputeTokenFormatFull})
	if err != nil {
		return nil, fmt.Errorf("the metadata server's ID token credentials: %w", err)
	}
	return creds, nil
}

// impersonatedIDTokenCredentials are ID token credentials for audience that
// impersonate the service account named in the external-account credential
// file at credPath, with the account's email in each token.
func impersonatedIDTokenCredentials(ctx context.Context, credPath, audience string) (*auth.Credentials, error) {
	credentialData, err := externalAccountCredentialJSON(ctx, credPath)
	if err != nil {
		return nil, err
	}

	var config externalAccountFile
	if err := json.Unmarshal(credentialData, &config); err != nil {
		return nil, fmt.Errorf("parse Google credentials: %w", err)
	}
	if config.Type != "external_account" {
		return nil, fmt.Errorf("google credential type %q cannot mint an impersonated ID token", config.Type)
	}
	principal, err := serviceAccountFromImpersonationURL(config.ServiceAccountImpersonationURL)
	if err != nil {
		return nil, err
	}

	var baseConfig map[string]json.RawMessage
	if err := json.Unmarshal(credentialData, &baseConfig); err != nil {
		return nil, fmt.Errorf("parse Google credentials: %w", err)
	}
	delete(baseConfig, "service_account_impersonation_url")
	baseCredentialData, err := json.Marshal(baseConfig)
	if err != nil {
		return nil, fmt.Errorf("prepare federated credentials: %w", err)
	}
	baseCredentials, err := credentials.NewCredentialsFromJSON(
		credentials.ExternalAccount,
		baseCredentialData,
		&credentials.DetectOptions{Scopes: []string{CloudPlatformScope}},
	)
	if err != nil {
		return nil, fmt.Errorf("detect federated credentials: %w", err)
	}
	idTokenCredentials, err := impersonate.NewIDTokenCredentials(&impersonate.IDTokenOptions{
		Audience:        audience,
		TargetPrincipal: principal,
		IncludeEmail:    true,
		Credentials:     baseCredentials,
	})
	if err != nil {
		return nil, fmt.Errorf("create ID token credentials: %w", err)
	}
	return idTokenCredentials, nil
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
