package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/genaiconfig"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
)

// gatewayCredentialCache keeps one gateway credential per credential file,
// so each ID token is fetched once and reused until it nears its expiry.
var gatewayCredentialCache sync.Map

// gatewayCredentials are the ID token credentials cfg signs in to the model
// gateway with: the metadata server's, or the account cfg's credential file
// impersonates.
func gatewayCredentials(ctx context.Context, cfg *config.Config) (*auth.Credentials, error) {
	credentialPath := cfg.GoogleCredentialsFile
	if cached, ok := gatewayCredentialCache.Load(credentialPath); ok {
		return cached.(*auth.Credentials), nil
	}
	credentials, err := googleauth.GatewayCredentials(ctx, credentialPath, googleauth.ModelGatewayAudience)
	if err != nil {
		return nil, fmt.Errorf("the model gateway's credentials: %w", err)
	}
	cached, _ := gatewayCredentialCache.LoadOrStore(credentialPath, credentials)
	return cached.(*auth.Credentials), nil
}

// usesModelGateway is whether cfg sends its Gemini calls to the model gateway.
func usesModelGateway(cfg *config.Config) bool {
	return strings.TrimSpace(cfg.ModelGatewayURL) != ""
}

// conciergeGenAIOptions are the concierge's model client options: the model
// gateway when cfg names one, else cfg's Gemini settings with Vertex AI
// credentials.
func conciergeGenAIOptions(ctx context.Context, cfg *config.Config) (*genaiconfig.Options, error) {
	options := genAIOptions(cfg, cfg.GeminiAPIKey)
	if usesModelGateway(cfg) {
		credentials, err := gatewayCredentials(ctx, cfg)
		if err != nil {
			return nil, err
		}
		options.GatewayURL, options.GatewayCredentials = cfg.ModelGatewayURL, credentials
		return options, nil
	}
	if err := addVertexCredentials(ctx, cfg, options); err != nil {
		return nil, err
	}
	return options, nil
}

// geminiVoiceCredentials are the credentials a Gemini voice signs its calls
// with, and the gateway URL to send them to: the gateway's ID token when cfg
// names a gateway, else a Vertex AI access token and no URL.
func geminiVoiceCredentials(ctx context.Context, cfg *config.Config) (*auth.Credentials, string, error) {
	if usesModelGateway(cfg) {
		credentials, err := gatewayCredentials(ctx, cfg)
		return credentials, cfg.ModelGatewayURL, err
	}
	credentials, err := googleauth.Detect(ctx, cfg.GoogleCredentialsFile, googleauth.CloudPlatformScope)
	if err != nil {
		return nil, "", fmt.Errorf("detect Google credentials: %w", err)
	}
	return credentials, "", nil
}
