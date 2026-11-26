package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/googleauth"
)

const piSecretsCommand = "pi-secrets"

const piSecretsClientCertificate = "GOOGLE_API_USE_CLIENT_CERTIFICATE"

const secretManagerAccessBase = "https://secretmanager.googleapis.com/v1"

// secretVersion is one Secret Manager version read through its access endpoint.
// Version is the numeric version Secret Manager named in its response.
type secretVersion struct {
	Version int
	Data    []byte
}

// secretReader reads a named Secret Manager version. Its only operation is
// access, so callers cannot list versions with its credentials.
type secretReader interface {
	Access(ctx context.Context, project, secret, version string) (secretVersion, error)
}

// signInCheck verifies that a temporary external-account credential file can
// obtain an access token with the candidate certificate it names.
type signInCheck func(ctx context.Context, credentialsFile string) error

type repeatedString []string

func (values *repeatedString) String() string { return strings.Join(*values, ",") }

func (values *repeatedString) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type environmentSecret struct {
	variable string
	secret   string
	value    []byte
	version  int
}

// runPiSecrets reads the requested deployment secrets without printing their
// values. It returns a process exit status so main can keep all diagnostics on
// stdout, which update.sh captures without exposing a secret in its arguments.
func runPiSecrets(ctx context.Context, args []string, stdout io.Writer, defaultCredentials func() (string, error), newReader func(context.Context, string) (secretReader, error), signIn signInCheck) int {
	if len(args) == 0 || args[0] != piSecretsCommand {
		return piSecretsFail(stdout, "expected %s", piSecretsCommand)
	}

	clientCertificatesOffForPiSecrets()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	flags := flag.NewFlagSet(piSecretsCommand, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "Google Cloud project")
	outDir := flags.String("out", "", "directory for deployment secrets")
	credentials := flags.String("credentials", "", "Google credential file")
	certificateSecret := flags.String("certificate-secret", "", "Secret Manager certificate secret")
	keyFile := flags.String("key", "", "device private key")
	installedCertificate := flags.String("installed-certificate", "", "installed device certificate")
	var environmentFlags repeatedString
	flags.Var(&environmentFlags, "env-secret", "environment variable and Secret Manager secret")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return piSecretsFail(stdout, "invalid arguments")
	}
	if *project == "" || *outDir == "" {
		return piSecretsFail(stdout, "--project and --out are required")
	}

	environment, err := parseEnvironmentSecrets(environmentFlags)
	if err != nil {
		return piSecretsFail(stdout, "%v", err)
	}
	hasCertificate := *certificateSecret != "" || *keyFile != "" || *installedCertificate != ""
	if hasCertificate && (*certificateSecret == "" || *keyFile == "" || *installedCertificate == "") {
		return piSecretsFail(stdout, "--certificate-secret, --key, and --installed-certificate must be set together")
	}
	if len(environment) == 0 && !hasCertificate {
		return piSecretsFail(stdout, "at least one --env-secret or certificate option is required")
	}

	credentialsSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "credentials" {
			credentialsSet = true
		}
	})
	credentialFile := *credentials
	if credentialsSet {
		if strings.TrimSpace(credentialFile) == "" {
			return piSecretsFail(stdout, "--credentials must name a file")
		}
	} else {
		credentialFile, err = defaultCredentials()
		if err != nil {
			return piSecretsFail(stdout, "could not load default credentials")
		}
	}

	reader, err := newReader(ctx, credentialFile)
	if err != nil {
		return piSecretsFail(stdout, "could not create Secret Manager reader")
	}
	code := 0
	if len(environment) != 0 {
		code = stageEnvironment(ctx, stdout, reader, *project, *outDir, environment)
	}
	// A failed read or check of an environment secret leaves the certificate
	// step to run: the two are independent, and only the environment sets the
	// exit code.
	if hasCertificate {
		stageCertificate(ctx, stdout, reader, signIn, credentialFile, *project, *certificateSecret, *keyFile, *installedCertificate, *outDir)
	}
	return code
}

// stageEnvironment reads and checks each environment secret, then writes
// secrets.env with every value, in flag order. It writes nothing when one read
// or check fails, and returns the exit code.
func stageEnvironment(ctx context.Context, stdout io.Writer, reader secretReader, project, outDir string, environment []environmentSecret) int {
	for index := range environment {
		secret := &environment[index]
		version, err := reader.Access(ctx, project, secret.secret, "latest")
		if err != nil {
			return piSecretsFail(stdout, "could not read %s from %s", secret.variable, secret.secret)
		}
		value := trimOneTrailingLineFeed(version.Data)
		if len(value) == 0 || bytes.ContainsAny(value, "\r\n\x00") {
			return piSecretsFail(stdout, "value for %s from %s is not safe for an environment file", secret.variable, secret.secret)
		}
		secret.value = value
		secret.version = version.Version
	}
	contents := make([]byte, 0)
	for _, secret := range environment {
		contents = append(contents, secret.variable...)
		contents = append(contents, '=')
		contents = append(contents, secret.value...)
		contents = append(contents, '\n')
	}
	if err := writePrivateFile(outDir, "secrets.env", contents); err != nil {
		return piSecretsFail(stdout, "could not write secrets.env")
	}
	for _, secret := range environment {
		_, _ = fmt.Fprintf(stdout, "pi-secrets: %s from %s version %d\n", secret.variable, secret.secret, secret.version)
	}
	return 0
}

func clientCertificatesOffForPiSecrets() {
	if _, set := os.LookupEnv(piSecretsClientCertificate); !set {
		_ = os.Setenv(piSecretsClientCertificate, "false")
	}
}

func parseEnvironmentSecrets(values []string) ([]environmentSecret, error) {
	secrets := make([]environmentSecret, 0, len(values))
	for _, value := range values {
		variable, secret, ok := strings.Cut(value, "=")
		if !ok || !validEnvironmentVariable(variable) || secret == "" {
			return nil, errors.New("--env-secret must be VARIABLE=SECRET")
		}
		secrets = append(secrets, environmentSecret{variable: variable, secret: secret})
	}
	return secrets, nil
}

func validEnvironmentVariable(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if (r >= 'A' && r <= 'Z') || r == '_' || (index != 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func trimOneTrailingLineFeed(value []byte) []byte {
	if len(value) != 0 && value[len(value)-1] == '\n' {
		return value[:len(value)-1]
	}
	return value
}

func piSecretsFail(stdout io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stdout, "pi-secrets: "+format+"\n", args...)
	return 1
}

func writePrivateFile(dir, name string, contents []byte) (err error) {
	temporary, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	temporaryPath = ""
	return nil
}

func stageCertificate(ctx context.Context, stdout io.Writer, reader secretReader, signIn signInCheck, credentialsFile, project, secret, keyFile, installedCertificate, outDir string) {
	installed, err := readSingleCertificate(installedCertificate)
	if err != nil {
		piSecretsCertificateProblem(stdout, 0, "installed certificate cannot be read")
		return
	}
	publicKey, err := readPublicKey(keyFile)
	if err != nil {
		piSecretsCertificateProblem(stdout, 0, "private key cannot be read")
		return
	}

	latest, err := reader.Access(ctx, project, secret, "latest")
	if err != nil {
		piSecretsCertificateProblem(stdout, 0, "latest certificate cannot be read")
		return
	}
	if latest.Version < 1 {
		piSecretsCertificateProblem(stdout, 0, "latest certificate has no numeric version")
		return
	}

	latestReason := "no newer certificate signs in"
	for offset := 0; offset < 10; offset++ {
		version := latest.Version - offset
		if version < 1 {
			break
		}
		candidate := latest
		if offset != 0 {
			candidate, err = reader.Access(ctx, project, secret, strconv.Itoa(version))
			if err != nil {
				if secretVersionUnavailable(err) {
					continue
				}
				break
			}
		}
		reason := certificateCandidateReason(candidate.Data, publicKey, installed)
		if reason == "" {
			reason = certificateSignsIn(ctx, credentialsFile, candidate.Data, keyFile, signIn)
		}
		if reason == "" {
			if err := writePrivateFile(outDir, "device.crt", candidate.Data); err != nil {
				if offset == 0 {
					latestReason = "newer certificate could not be staged"
				}
				break
			}
			_, _ = fmt.Fprintf(stdout, "pi-secrets: certificate version %d, not after %s: newer and signs in\n", candidate.Version, certificateNotAfter(candidate.Data))
			return
		}
		if offset == 0 {
			latestReason = reason
		}
	}
	piSecretsCertificateProblem(stdout, latest.Version, latestReason)
}

func piSecretsCertificateProblem(stdout io.Writer, version int, reason string) {
	if version > 0 {
		_, _ = fmt.Fprintf(stdout, "pi-secrets: certificate version %d kept: %s\n", version, reason)
		return
	}
	_, _ = fmt.Fprintf(stdout, "pi-secrets: certificate kept: %s\n", reason)
}

func certificateCandidateReason(data []byte, publicKey crypto.PublicKey, installed *x509.Certificate) string {
	candidate, err := parseSingleCertificate(data)
	if err != nil {
		return "candidate is not exactly one PEM certificate"
	}
	matches, err := samePublicKey(candidate.PublicKey, publicKey)
	if err != nil || !matches {
		return "candidate public key does not match --key"
	}
	if !candidate.NotAfter.After(installed.NotAfter) {
		return "not newer"
	}
	return ""
}

func certificateNotAfter(data []byte) string {
	certificate, err := parseSingleCertificate(data)
	if err != nil {
		return "unknown"
	}
	return certificate.NotAfter.UTC().Format(time.RFC3339)
}

func readSingleCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseSingleCertificate(data)
}

func parseSingleCertificate(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("not exactly one certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func readPublicKey(path string) (crypto.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("not exactly one private key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer.Public(), nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &key.PublicKey, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return &key.PublicKey, nil
	}
	return nil, errors.New("unsupported private key")
}

func samePublicKey(first, second crypto.PublicKey) (bool, error) {
	firstDER, err := x509.MarshalPKIXPublicKey(first)
	if err != nil {
		return false, err
	}
	secondDER, err := x509.MarshalPKIXPublicKey(second)
	if err != nil {
		return false, err
	}
	return bytes.Equal(firstDER, secondDER), nil
}

func certificateSignsIn(ctx context.Context, credentialsFile string, candidate []byte, keyFile string, signIn signInCheck) string {
	dir, err := os.MkdirTemp("", "ghost-wispr-pi-secrets-")
	if err != nil {
		return "candidate sign-in could not start"
	}
	defer func() { _ = os.RemoveAll(dir) }()

	candidatePath := filepath.Join(dir, "candidate.pem")
	if err := os.WriteFile(candidatePath, candidate, 0o600); err != nil {
		return "candidate sign-in could not start"
	}
	certificateConfigPath := filepath.Join(dir, "certificate_config.json")
	certificateConfig, err := json.Marshal(map[string]any{
		"cert_configs": map[string]any{
			"workload": map[string]string{
				"cert_path": candidatePath,
				"key_path":  keyFile,
			},
		},
	})
	if err != nil {
		return "candidate sign-in could not start"
	}
	if err := os.WriteFile(certificateConfigPath, certificateConfig, 0o600); err != nil {
		return "candidate sign-in could not start"
	}

	credentialsData, err := os.ReadFile(credentialsFile)
	if err != nil {
		return "candidate credentials cannot be read"
	}
	var credentials map[string]any
	if err := json.Unmarshal(credentialsData, &credentials); err != nil {
		return "candidate credentials cannot be read"
	}
	certificate, ok := nestedMap(credentials, "credential_source", "certificate")
	if !ok {
		return "candidate credentials have no certificate source"
	}
	certificate["certificate_config_location"] = certificateConfigPath
	temporaryCredentials, err := json.Marshal(credentials)
	if err != nil {
		return "candidate credentials cannot be written"
	}
	temporaryCredentialsPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(temporaryCredentialsPath, temporaryCredentials, 0o600); err != nil {
		return "candidate credentials cannot be written"
	}
	if err := signIn(ctx, temporaryCredentialsPath); err != nil {
		return "candidate does not sign in"
	}
	return ""
}

func nestedMap(value map[string]any, keys ...string) (map[string]any, bool) {
	current := value
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

type secretManagerReader struct {
	token  func(context.Context) (string, error)
	client *http.Client
}

func newSecretManagerReader(ctx context.Context, credentialsFile string) (secretReader, error) {
	credentials, err := googleauth.Detect(ctx, credentialsFile, googleauth.CloudPlatformScope)
	if err != nil {
		return nil, err
	}
	return &secretManagerReader{
		token: func(ctx context.Context) (string, error) {
			token, err := credentials.Token(ctx)
			if err != nil {
				return "", err
			}
			return token.Value, nil
		},
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (reader *secretManagerReader) Access(ctx context.Context, project, secret, version string) (secretVersion, error) {
	token, err := reader.token(ctx)
	if err != nil {
		return secretVersion{}, err
	}
	endpoint := secretManagerAccessBase + "/projects/" + url.PathEscape(project) + "/secrets/" + url.PathEscape(secret) + "/versions/" + url.PathEscape(version) + ":access"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return secretVersion{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := reader.client.Do(request)
	if err != nil {
		return secretVersion{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return secretVersion{}, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return secretVersion{}, newSecretManagerError(response.StatusCode, body)
	}

	var payload struct {
		Name    string `json:"name"`
		Payload struct {
			Data       string `json:"data"`
			DataCRC32C string `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return secretVersion{}, err
	}
	versionNumber, err := secretManagerVersion(payload.Name)
	if err != nil {
		return secretVersion{}, err
	}
	data, err := base64.StdEncoding.DecodeString(payload.Payload.Data)
	if err != nil {
		return secretVersion{}, err
	}
	wantCRC, err := strconv.ParseUint(payload.Payload.DataCRC32C, 10, 64)
	if err != nil || uint64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))) != wantCRC {
		return secretVersion{}, errors.New("secret manager payload CRC32C did not match")
	}
	return secretVersion{Version: versionNumber, Data: data}, nil
}

func secretManagerVersion(name string) (int, error) {
	const marker = "/versions/"
	index := strings.LastIndex(name, marker)
	if index < 0 {
		return 0, errors.New("secret manager access response omitted its version")
	}
	version, err := strconv.Atoi(name[index+len(marker):])
	if err != nil || version < 1 {
		return 0, errors.New("secret manager access response has an invalid version")
	}
	return version, nil
}

type secretManagerAPIError struct {
	status int
	code   string
}

func (err *secretManagerAPIError) Error() string {
	if err.code != "" {
		return err.code
	}
	return fmt.Sprintf("secret manager answered HTTP %d", err.status)
}

func newSecretManagerError(status int, body []byte) error {
	var response struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	return &secretManagerAPIError{status: status, code: response.Error.Status}
}

func secretVersionUnavailable(err error) bool {
	if code := secretVersionErrorCode(err); code == "FAILED_PRECONDITION" || code == "NOT_FOUND" {
		return true
	}
	return false
}

func secretVersionErrorCode(err error) string {
	var secretError *secretManagerAPIError
	if errors.As(err, &secretError) {
		return secretError.code
	}
	return ""
}

func productionSignInCheck(ctx context.Context, credentialsFile string) error {
	credentials, err := googleauth.Detect(ctx, credentialsFile, googleauth.CloudPlatformScope)
	if err != nil {
		return err
	}
	_, err = credentials.Token(ctx)
	return err
}
