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
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
)

const piSecretsCommand = "pi-secrets"

const piSecretsClientCertificate = "GOOGLE_API_USE_CLIENT_CERTIFICATE"

const secretManagerAccessBase = "https://secretmanager.googleapis.com/v1"

// piSecretsBound is how long one pi-secrets run may take: every read, check
// and sign-in together.
const piSecretsBound = 30 * time.Second

// certificateVersionsRead is how many versions of the certificate secret the
// certificate step reads at most: latest and the nine under it, or the ten
// above the installed version.
const certificateVersionsRead = 10

// secretVersion is one Secret Manager version read through its access call.
// Version, at least 1, is the version Secret Manager's answer names, so a read
// of latest says which version it was.
type secretVersion struct {
	Version int
	Data    []byte
}

// secretReader reads one version of a Secret Manager secret by its access
// call, by number or as latest. It has no call that lists versions, and the
// readers' role, Secret Accessor, holds no versions.list permission, so IAM
// would refuse a listing. The production reader checks the CRC32C that Secret
// Manager sends with each value.
type secretReader interface {
	Access(ctx context.Context, project, secret, version string) (secretVersion, error)
}

// signInCheck verifies that a temporary external-account credential file can
// obtain an access token with the candidate certificate it names.
type signInCheck func(ctx context.Context, credentialsFile string) error

// Why a read of the production reader failed, for a reason pi-secrets may
// print: its sign-in, a value whose CRC32C does not match, or an answer it
// cannot read. A refusal is a *secretManagerAPIError.
var (
	errSecretManagerSignIn   = errors.New("sign-in failed")
	errSecretManagerChecksum = errors.New("CRC32C mismatch")
	errSecretManagerAnswer   = errors.New("malformed answer")
)

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

// piSecretsRequest is a pi-secrets run's arguments.
type piSecretsRequest struct {
	project string
	outDir  string
	// credentialsFile is --credentials, or "" when the flag is absent.
	credentialsFile string
	environment     []environmentSecret
	// certificate is the certificate step's arguments, or nil without them.
	certificate *certificateRequest
}

// certificateRequest is the certificate step's arguments.
type certificateRequest struct {
	project              string
	secret               string
	keyFile              string
	installedCertificate string
	// installedVersion is the installed certificate's version, or 0 when
	// --installed-certificate-version is absent.
	installedVersion int
	credentialsFile  string
	outDir           string
}

// runPiSecretsCommand is ghost-wispr pi-secrets as main runs it. It reads the
// config at configPath for google_credentials_file only when --credentials is
// absent, reads Secret Manager over HTTPS, and checks a candidate certificate
// by a real sign-in.
func runPiSecretsCommand(ctx context.Context, args []string, stdout io.Writer, configPath string) int {
	defaultCredentials := func() (string, error) {
		cfg, _, err := config.Load(configPath)
		if err != nil {
			return "", err
		}
		return cfg.GoogleCredentialsFile, nil
	}
	return runPiSecrets(ctx, args, stdout, defaultCredentials, newSecretManagerReader, productionSignInCheck)
}

// runPiSecrets reads the requested deployment secrets without printing their
// values. It returns a process exit status so main can keep all diagnostics on
// stdout, which update.sh captures without exposing a secret in its arguments:
// 2 for an argument error, 1 when a key's read or check fails, and 0 otherwise.
// A certificate problem never changes it.
func runPiSecrets(ctx context.Context, args []string, stdout io.Writer, defaultCredentials func() (string, error), newReader func(context.Context, string) (secretReader, error), signIn signInCheck) int {
	if len(args) == 0 || args[0] != piSecretsCommand {
		return piSecretsUsage(stdout, "expected %s", piSecretsCommand)
	}

	clientCertificatesOffForPiSecrets()
	ctx, cancel := context.WithTimeout(ctx, piSecretsBound)
	defer cancel()

	request, err := parsePiSecretsArguments(args[1:])
	if err != nil {
		return piSecretsUsage(stdout, "%v", err)
	}
	credentialsFile := request.credentialsFile
	if credentialsFile == "" {
		// No Application Default Credentials: on the Pi they would be whatever
		// its environment points at.
		credentialsFile, err = defaultCredentials()
		if err != nil {
			return piSecretsFail(stdout, "could not read the config's google_credentials_file; pass --credentials")
		}
		if strings.TrimSpace(credentialsFile) == "" {
			return piSecretsFail(stdout, "the config names no google_credentials_file; pass --credentials")
		}
	}

	reader, err := newReader(ctx, credentialsFile)
	if err != nil {
		return piSecretsFail(stdout, "could not create Secret Manager reader")
	}
	code := 0
	if len(request.environment) != 0 {
		code = stageEnvironment(ctx, stdout, reader, request.project, request.outDir, request.environment)
	}
	// A failed read or check of an environment secret leaves the certificate
	// step to run: the two are independent, and only the environment sets the
	// exit code.
	if request.certificate != nil {
		request.certificate.credentialsFile = credentialsFile
		stageCertificate(ctx, stdout, reader, signIn, request.certificate)
	}
	return code
}

// parsePiSecretsArguments reads the arguments after pi-secrets. Its error is
// an argument error, which names no value.
func parsePiSecretsArguments(args []string) (*piSecretsRequest, error) {
	flags := flag.NewFlagSet(piSecretsCommand, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "Google Cloud project")
	outDir := flags.String("out", "", "directory for deployment secrets")
	credentials := flags.String("credentials", "", "Google credential file")
	certificateSecret := flags.String("certificate-secret", "", "Secret Manager certificate secret")
	keyFile := flags.String("key", "", "device private key")
	installedCertificate := flags.String("installed-certificate", "", "installed device certificate")
	installedVersion := flags.String("installed-certificate-version", "", "installed device certificate's version")
	var environmentFlags repeatedString
	flags.Var(&environmentFlags, "env-secret", "environment variable and Secret Manager secret")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return nil, errors.New("invalid arguments")
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if *project == "" || *outDir == "" {
		return nil, errors.New("--project and --out are required")
	}
	if set["credentials"] && strings.TrimSpace(*credentials) == "" {
		return nil, errors.New("--credentials must name a file")
	}
	environment, err := parseEnvironmentSecrets(environmentFlags)
	if err != nil {
		return nil, err
	}
	request := &piSecretsRequest{project: *project, outDir: *outDir, credentialsFile: *credentials, environment: environment}
	if set["certificate-secret"] || set["key"] || set["installed-certificate"] || set["installed-certificate-version"] {
		if *certificateSecret == "" || *keyFile == "" || *installedCertificate == "" {
			return nil, errors.New("--certificate-secret, --key, and --installed-certificate must be set together")
		}
		version := 0
		if set["installed-certificate-version"] {
			version, err = strconv.Atoi(*installedVersion)
			if err != nil || version < 1 || version > math.MaxInt-certificateVersionsRead {
				return nil, errors.New("--installed-certificate-version must be a positive integer")
			}
		}
		request.certificate = &certificateRequest{
			project:              *project,
			secret:               *certificateSecret,
			keyFile:              *keyFile,
			installedCertificate: *installedCertificate,
			installedVersion:     version,
			outDir:               *outDir,
		}
	}
	if len(environment) == 0 && request.certificate == nil {
		return nil, errors.New("at least one --env-secret or certificate option is required")
	}
	return request, nil
}

// stageEnvironment reads and checks each environment secret, then writes
// secrets.env with every value, in flag order. It writes nothing when one read
// or check fails, and returns the exit code.
func stageEnvironment(ctx context.Context, stdout io.Writer, reader secretReader, project, outDir string, environment []environmentSecret) int {
	for index := range environment {
		secret := &environment[index]
		version, err := reader.Access(ctx, project, secret.secret, "latest")
		if err != nil {
			return piSecretsFail(stdout, "could not read %s from %s: %s", secret.variable, secret.secret, secretReadCode(err))
		}
		value := trimOneTrailingLineFeed(version.Data)
		if !environmentValueSafe(value) {
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

// environmentValueSafe reports whether value reads back as itself from a
// VARIABLE=value line in both readers of the Pi's .env: systemd's
// EnvironmentFile= parser and the recorder's own (config.loadEnvFile). So it
// refuses an empty value; a carriage return, a line feed or a NUL, which end
// the line; a backslash, which escapes the next character and at the end
// joins the next line to the value; a leading quote, which starts a quoted
// value; and whitespace at either end, which both readers drop.
func environmentValueSafe(value []byte) bool {
	if len(value) == 0 || bytes.ContainsAny(value, "\r\n\x00\\") || value[0] == '\'' || value[0] == '"' {
		return false
	}
	first, _ := utf8.DecodeRune(value)
	last, _ := utf8.DecodeLastRune(value)
	return !unicode.IsSpace(first) && !unicode.IsSpace(last)
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

// piSecretsFail prints why a read or check failed and returns 1.
func piSecretsFail(stdout io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stdout, "pi-secrets: "+format+"\n", args...)
	return 1
}

// piSecretsUsage prints an argument error and returns 2, a usage error's exit
// code, apart from a failed read's 1.
func piSecretsUsage(stdout io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stdout, "pi-secrets: "+format+"\n", args...)
	return 2
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

// stageCertificate stages, as device.crt in the request's out directory, the
// newest version of the certificate secret that passes every check, or prints
// why it kept the installed certificate. It reads latest and walks down from
// it; when latest is disabled, destroyed or absent, and the installed version
// is known, it walks up from that version instead.
func stageCertificate(ctx context.Context, stdout io.Writer, reader secretReader, signIn signInCheck, request *certificateRequest) {
	installed, err := readInstalledCertificate(request.installedCertificate)
	if err != nil {
		certificateKept(stdout, 0, "installed certificate cannot be read")
		return
	}
	publicKey, err := readPublicKey(request.keyFile)
	if err != nil {
		certificateKept(stdout, 0, "private key cannot be read")
		return
	}
	check := func(candidate secretVersion) string {
		if reason := certificateCandidateReason(candidate.Data, publicKey, installed); reason != "" {
			return reason
		}
		return certificateSignsIn(ctx, request.credentialsFile, candidate.Data, request.keyFile, signIn)
	}
	stage := func(candidate secretVersion) bool {
		if err := writePrivateFile(request.outDir, "device.crt", candidate.Data); err != nil {
			return false
		}
		_, _ = fmt.Fprintf(stdout, "pi-secrets: certificate version %d, not after %s: newer and signs in\n", candidate.Version, certificateNotAfter(candidate.Data))
		return true
	}

	latest, err := reader.Access(ctx, request.project, request.secret, "latest")
	var version int
	var reason string
	switch {
	case err == nil:
		version, reason = walkCertificatesDown(ctx, reader, request, latest, check, stage)
	case request.installedVersion != 0 && secretVersionUnavailable(err):
		version, reason = walkCertificatesUp(ctx, reader, request, "latest certificate cannot be read: "+secretReadCode(err), check, stage)
	default:
		reason = "latest certificate cannot be read: " + secretReadCode(err)
	}
	if reason != "" {
		certificateKept(stdout, version, reason)
	}
}

// walkCertificatesDown checks latest, then up to nine versions under it,
// newest first, and stages the first that passes. It skips a version that is
// disabled, destroyed or absent, and stops at any other read failure, a failed
// staging, or the end of the run's bound. It returns latest's version and why
// it kept the installed certificate: the failure that stopped it, or else
// latest's own reason; the reason is "" once it staged a version.
func walkCertificatesDown(ctx context.Context, reader secretReader, request *certificateRequest, latest secretVersion, check func(secretVersion) string, stage func(secretVersion) bool) (int, string) {
	if latest.Version < 1 {
		return 0, "latest certificate has no numeric version"
	}
	latestReason := ""
	for offset := range certificateVersionsRead {
		version := latest.Version - offset
		if version < 1 {
			break
		}
		candidate := latest
		if offset != 0 {
			var err error
			candidate, err = reader.Access(ctx, request.project, request.secret, strconv.Itoa(version))
			if err != nil {
				if secretVersionUnavailable(err) {
					continue
				}
				return latest.Version, fmt.Sprintf("version %d cannot be read: %s", version, secretReadCode(err))
			}
		}
		reason := check(candidate)
		if reason == "" {
			if stage(candidate) {
				return latest.Version, ""
			}
			return latest.Version, fmt.Sprintf("version %d could not be staged", candidate.Version)
		}
		if offset == 0 {
			latestReason = reason
		}
		if ctx.Err() != nil {
			return latest.Version, reason
		}
	}
	return latest.Version, latestReason
}

// walkCertificatesUp reads up to ten versions above the installed one, oldest
// first, when latest cannot be read because it is disabled, destroyed or
// absent: a renewal can sit under such a latest. It skips a disabled or
// destroyed version, and stops at an absent one or any other read failure.
// Then it stages the newest version read that passes. It returns the newest
// version read and why it kept the installed certificate: the read failure
// that stopped it, or else that version's own reason; with no version read,
// latestReason and why. The reason is "" once it staged a version.
func walkCertificatesUp(ctx context.Context, reader secretReader, request *certificateRequest, latestReason string, check func(secretVersion) string, stage func(secretVersion) bool) (int, string) {
	var candidates []secretVersion
	stopped := ""
walk:
	for version := request.installedVersion + 1; version <= request.installedVersion+certificateVersionsRead; version++ {
		candidate, err := reader.Access(ctx, request.project, request.secret, strconv.Itoa(version))
		if err == nil {
			candidates = append(candidates, candidate)
			continue
		}
		switch code := secretReadCode(err); code {
		case "FAILED_PRECONDITION":
			continue
		case "NOT_FOUND":
		default:
			stopped = fmt.Sprintf("version %d cannot be read: %s", version, code)
		}
		break walk
	}
	if len(candidates) == 0 {
		if stopped == "" {
			stopped = fmt.Sprintf("no readable version above %d", request.installedVersion)
		}
		return 0, latestReason + "; " + stopped
	}
	newest := candidates[len(candidates)-1].Version
	reason := stopped
	for index := len(candidates) - 1; index >= 0; index-- {
		candidateReason := check(candidates[index])
		if candidateReason == "" {
			if stage(candidates[index]) {
				return newest, ""
			}
			return newest, fmt.Sprintf("version %d could not be staged", candidates[index].Version)
		}
		if ctx.Err() != nil {
			return newest, candidateReason
		}
		if reason == "" {
			reason = candidateReason
		}
	}
	return newest, reason
}

// certificateKept prints why the certificate step kept the installed
// certificate, naming version when it is known.
func certificateKept(stdout io.Writer, version int, reason string) {
	if version > 0 {
		_, _ = fmt.Fprintf(stdout, "pi-secrets: certificate version %d kept: %s\n", version, reason)
		return
	}
	_, _ = fmt.Fprintf(stdout, "pi-secrets: certificate kept: %s\n", reason)
}

func certificateCandidateReason(data []byte, publicKey crypto.PublicKey, installed *x509.Certificate) string {
	candidate, err := parseCandidateCertificate(data)
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
	certificate, err := parseCandidateCertificate(data)
	if err != nil {
		return "unknown"
	}
	return certificate.NotAfter.UTC().Format(time.RFC3339)
}

// parseCandidateCertificate parses a version of the certificate secret as the
// file update.sh would install: exactly one PEM CERTIFICATE block with no
// headers, and nothing before or after it but whitespace.
func parseCandidateCertificate(data []byte) (*x509.Certificate, error) {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN CERTIFICATE-----")) {
		return nil, errors.New("not a PEM certificate")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("not exactly one certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// readInstalledCertificate reads the installed certificate: a PEM file whose
// one CERTIFICATE block has nothing after it.
func readInstalledCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("not exactly one certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// readPublicKey reads the public key of the private key in the PEM file at
// path: one PKCS #8, PKCS #1 or SEC 1 key block, after any EC PARAMETERS
// blocks, which openssl ecparam writes before a key unless told -noout. These
// are the files tls.LoadX509KeyPair, and so the sign-in, reads.
func readPublicKey(path string) (crypto.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no private key")
		}
		if block.Type == "EC PARAMETERS" {
			continue
		}
		if len(bytes.TrimSpace(rest)) != 0 {
			return nil, errors.New("more than one private key")
		}
		return privateKeyPublicKey(block)
	}
}

// privateKeyPublicKey is the public key of the private key in block.
func privateKeyPublicKey(block *pem.Block) (crypto.PublicKey, error) {
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, errors.New("unsupported private key")
		}
		return signer.Public(), nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		return &key.PublicKey, nil
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		return &key.PublicKey, nil
	}
	return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
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

// certificateSignsIn signs in with candidate and the key at keyFile, through
// a temporary copy of the credential file whose certificate source names them,
// and returns why the candidate failed, or "" when it signed in. It removes
// its temporary directory on return.
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
	// The token exchange refuses a certificate source that names both.
	delete(certificate, "use_default_certificate_config")
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
		if ctx.Err() != nil {
			return "candidate sign-in ran out of time"
		}
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
		client: &http.Client{Timeout: piSecretsBound},
	}, nil
}

// Access reads version of secret by Secret Manager's access call, and checks
// the value against the CRC32C the answer gives. Once ctx has ended, it
// returns ctx's error, whichever step it was in.
func (reader *secretManagerReader) Access(ctx context.Context, project, secret, version string) (secretVersion, error) {
	token, err := reader.token(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return secretVersion{}, ctx.Err()
		}
		return secretVersion{}, fmt.Errorf("%w: %w", errSecretManagerSignIn, err)
	}
	endpoint := secretManagerAccessBase + "/projects/" + url.PathEscape(project) + "/secrets/" + url.PathEscape(secret) + "/versions/" + url.PathEscape(version) + ":access"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return secretVersion{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := reader.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return secretVersion{}, ctx.Err()
		}
		return secretVersion{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return secretVersion{}, ctx.Err()
		}
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
		return secretVersion{}, fmt.Errorf("%w: %w", errSecretManagerAnswer, err)
	}
	versionNumber, err := secretManagerVersion(payload.Name)
	if err != nil {
		return secretVersion{}, fmt.Errorf("%w: %w", errSecretManagerAnswer, err)
	}
	data, err := base64.StdEncoding.DecodeString(payload.Payload.Data)
	if err != nil {
		return secretVersion{}, fmt.Errorf("%w: %w", errSecretManagerAnswer, err)
	}
	wantCRC, err := strconv.ParseUint(payload.Payload.DataCRC32C, 10, 64)
	if err != nil || uint64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))) != wantCRC {
		return secretVersion{}, errSecretManagerChecksum
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

// secretManagerAPIError is an answer of Secret Manager's other than 2xx: its
// HTTP status, and its status code (such as PERMISSION_DENIED) when the answer
// gave one in that form.
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
	code := response.Error.Status
	if !statusCodeShaped(code) {
		code = ""
	}
	return &secretManagerAPIError{status: status, code: code}
}

// statusCodeShaped reports whether code looks like a Google API status code,
// such as PERMISSION_DENIED: capitals and underscores, at most 40 of them. A
// status of another shape is not printed.
func statusCodeShaped(code string) bool {
	if code == "" || len(code) > 40 {
		return false
	}
	for _, r := range code {
		if (r < 'A' || r > 'Z') && r != '_' {
			return false
		}
	}
	return true
}

// secretReadCode names why a read failed, in words pi-secrets may print: a
// Secret Manager status code, the HTTP status of an answer that gave none,
// DEADLINE_EXCEEDED or CANCELLED when the run's context ended, or a fixed
// phrase. It never gives the error's text, which can quote an answer.
func secretReadCode(err error) string {
	var refusal *secretManagerAPIError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "DEADLINE_EXCEEDED"
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	case errors.As(err, &refusal):
		if refusal.code != "" {
			return refusal.code
		}
		return fmt.Sprintf("HTTP %d", refusal.status)
	case errors.Is(err, errSecretManagerSignIn):
		return errSecretManagerSignIn.Error()
	case errors.Is(err, errSecretManagerChecksum):
		return errSecretManagerChecksum.Error()
	case errors.Is(err, errSecretManagerAnswer):
		return errSecretManagerAnswer.Error()
	}
	return "request failed"
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
