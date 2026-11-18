// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	natsjwt "github.com/nats-io/jwt/v2"
	"github.com/uptrace/opentelemetry-go-extra/otelzap"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/config"
	"github.com/NVIDIA/dsx-exchange/auth-callout/src/internal/observability/metrics"
)

// svidUsername routes an MQTT client's username and password connection to the
// SVID authenticator. The password carries the JWT-SVID. NATS clients send the
// JWT-SVID as a token instead.
const svidUsername = "svid"

// roleResourceType marks the dsx.nvidia.com/resource entry that names the
// workload's role.
const roleResourceType = "role"

// svidFailureReason labels auth_svid_failures_total. Only the constants below
// are valid values, so the metric stays bounded.
type svidFailureReason string

const (
	svidReasonMissingToken    svidFailureReason = "missing_token"
	svidReasonInvalidToken    svidFailureReason = "invalid_token"
	svidReasonExpired         svidFailureReason = "token_expired"
	svidReasonInvalidAudience svidFailureReason = "invalid_audience"
	svidReasonInvalidIssuer   svidFailureReason = "invalid_issuer"
	svidReasonInvalidClaims   svidFailureReason = "invalid_claims"
	svidReasonNoRule          svidFailureReason = "no_rule"
)

// svidClaims are the claims of a DSX JWT-SVID, as issued by the DSX Identity
// Keys service.
type svidClaims struct {
	jwt.RegisteredClaims
	Tenant    string         `json:"dsx.nvidia.com/tenant"`
	Region    string         `json:"dsx.nvidia.com/region"`
	Service   string         `json:"dsx.nvidia.com/service"`
	Partition string         `json:"dsx.nvidia.com/partition"`
	Resource  []svidResource `json:"dsx.nvidia.com/resource"`
}

// svidResource is one entry of the dsx.nvidia.com/resource claim, such as
// {"resource_type": "role", "resource_id": "svid-test"}.
type svidResource struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

// SVIDConfig sets which DSX JWT-SVIDs the SVID authenticator trusts. Leaving
// the svid block out of the service config turns SVID authentication off.
type SVIDConfig struct {
	JWKSURL           string   `koanf:"jwks-url"`
	AllowInsecure     bool     `koanf:"allow-insecure"`
	Issuers           []string `koanf:"issuers"`
	Audience          string   `koanf:"audience"`
	SigningAlgorithms []string `koanf:"signing-algorithms"`
}

// SVIDAuthenticator authenticates workloads by their DSX JWT-SVID. It matches
// the token's trust domain claims and role against the SVID permission entries
// and refuses tokens that match none.
type SVIDAuthenticator struct {
	jwks              keyfunc.Keyfunc
	pm                *config.PermissionsManager
	issuers           map[string]struct{}
	audience          string
	signingAlgorithms []string
	logger            *otelzap.Logger
	attempts          metric.Int64Counter
	failures          metric.Int64Counter
	cancel            context.CancelFunc
}

// NewSVIDAuthenticator creates an SVID authenticator that trusts tokens signed
// by a key from cfg.JWKSURL, issued by one of cfg.Issuers, for cfg.Audience.
func NewSVIDAuthenticator(cfg SVIDConfig, pm *config.PermissionsManager, logger *otelzap.Logger, serviceName string) (*SVIDAuthenticator, error) {
	if len(cfg.Issuers) == 0 {
		return nil, errors.New("SVID issuers are required")
	}
	issuerSet := make(map[string]struct{}, len(cfg.Issuers))
	for _, issuer := range cfg.Issuers {
		if issuer == "" {
			return nil, errors.New("SVID issuers must not be empty")
		}
		issuerSet[issuer] = struct{}{}
	}
	if cfg.Audience == "" {
		return nil, errors.New("SVID audience is required")
	}
	if err := validateSigningAlgorithms("SVID", cfg.SigningAlgorithms); err != nil {
		return nil, err
	}

	meter := metrics.GetMeter(serviceName)
	attempts, err := meter.Int64Counter("auth_svid_attempts_total",
		metric.WithDescription("Total SVID authentication attempts"))
	if err != nil {
		return nil, fmt.Errorf("failed to create SVID attempts counter: %w", err)
	}
	failures, err := meter.Int64Counter("auth_svid_failures_total",
		metric.WithDescription("Total SVID authentication failures"))
	if err != nil {
		return nil, fmt.Errorf("failed to create SVID failures counter: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	k, err := newJWKS(ctx, cfg.JWKSURL, cfg.AllowInsecure)
	if err != nil {
		cancel()
		return nil, err
	}

	logger.Info("SVID authenticator initialized",
		zap.String("jwks_url", cfg.JWKSURL),
		zap.Strings("issuers", cfg.Issuers),
		zap.String("audience", cfg.Audience),
		zap.Strings("signing_algorithms", cfg.SigningAlgorithms),
	)

	return &SVIDAuthenticator{
		jwks:              k,
		pm:                pm,
		issuers:           issuerSet,
		audience:          cfg.Audience,
		signingAlgorithms: slices.Clone(cfg.SigningAlgorithms),
		logger:            logger,
		attempts:          attempts,
		failures:          failures,
		cancel:            cancel,
	}, nil
}

// CanAuthenticate claims every MQTT connection with username "svid", with or
// without a token, and every NATS token from a trusted SVID issuer.
func (s *SVIDAuthenticator) CanAuthenticate(rc *natsjwt.AuthorizationRequestClaims) bool {
	if rc.ConnectOptions.Username == svidUsername && rc.ClientInformation.Type == "mqtt" {
		return true
	}
	return rc.ConnectOptions.Token != "" && s.hasTrustedIssuer(rc.ConnectOptions.Token)
}

// hasTrustedIssuer reads the issuer of a token without verifying it. It only
// routes the token. TryAuthenticate verifies it.
func (s *SVIDAuthenticator) hasTrustedIssuer(token string) bool {
	claims := &jwt.RegisteredClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return false
	}
	_, ok := s.issuers[claims.Issuer]
	return ok
}

// TryAuthenticate verifies the JWT-SVID and returns the profile of the entry
// that matches its identity.
func (s *SVIDAuthenticator) TryAuthenticate(ctx context.Context, rc *natsjwt.AuthorizationRequestClaims) (config.UserProfile, error) {
	s.attempts.Add(ctx, 1, metric.WithAttributes(attribute.String("method", "svid")))

	token := rc.ConnectOptions.Token
	if token == "" && rc.ConnectOptions.Username == svidUsername {
		token = rc.ConnectOptions.Password
	}
	if token == "" {
		return s.fail(ctx, svidReasonMissingToken, "", errors.New("SVID connection has no token"))
	}

	claims := &svidClaims{}
	_, err := jwt.ParseWithClaims(
		token,
		claims,
		s.jwks.Keyfunc,
		jwt.WithValidMethods(s.signingAlgorithms),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(s.audience),
	)
	if err != nil {
		return s.fail(ctx, parseFailureReason(err), "", fmt.Errorf("failed to verify SVID: %w", err))
	}
	if _, ok := s.issuers[claims.Issuer]; !ok {
		return s.fail(ctx, svidReasonInvalidIssuer, claims.Subject,
			fmt.Errorf("SVID issuer %q is not trusted", claims.Issuer))
	}

	key, err := claims.key()
	if err != nil {
		return s.fail(ctx, svidReasonInvalidClaims, claims.Subject, err)
	}

	profile, ok := s.pm.GetSVIDProfile(key)
	if !ok {
		return s.fail(ctx, svidReasonNoRule, claims.Subject,
			fmt.Errorf("no SVID rule matches %s", claims.Subject))
	}

	s.logger.Info("SVID authentication successful",
		zap.String("spiffe_id", claims.Subject),
		zap.String("rule", profile.Name),
		zap.String("account", profile.Account),
	)
	profile.Name = claims.Subject
	return profile, nil
}

// Close stops refreshing the key set.
func (s *SVIDAuthenticator) Close() error {
	s.cancel()
	return nil
}

func (s *SVIDAuthenticator) fail(ctx context.Context, reason svidFailureReason, spiffeID string, err error) (config.UserProfile, error) {
	s.failures.Add(ctx, 1, metric.WithAttributes(
		attribute.String("method", "svid"),
		attribute.String("reason", string(reason)),
	))
	s.logger.Warn("SVID authentication failed",
		zap.String("reason", string(reason)),
		zap.String("spiffe_id", spiffeID),
		zap.Error(err),
	)
	return config.UserProfile{}, err
}

// key returns the identity the claims assert. It requires every trust domain
// claim and a subject, and allows at most one role.
func (c *svidClaims) key() (config.SVIDKey, error) {
	if c.Subject == "" || c.Tenant == "" || c.Region == "" || c.Service == "" || c.Partition == "" {
		return config.SVIDKey{}, errors.New("SVID must carry sub, tenant, region, service and partition claims")
	}

	key := config.SVIDKey{
		Tenant:    c.Tenant,
		Region:    c.Region,
		Service:   c.Service,
		Partition: c.Partition,
	}
	for _, resource := range c.Resource {
		if resource.ResourceType != roleResourceType {
			continue
		}
		if key.Role != "" {
			return config.SVIDKey{}, fmt.Errorf("SVID %s carries more than one role", c.Subject)
		}
		if resource.ResourceID == "" {
			return config.SVIDKey{}, fmt.Errorf("SVID %s carries an empty role", c.Subject)
		}
		key.Role = resource.ResourceID
	}
	return key, nil
}

func parseFailureReason(err error) svidFailureReason {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return svidReasonExpired
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return svidReasonInvalidAudience
	default:
		return svidReasonInvalidToken
	}
}
