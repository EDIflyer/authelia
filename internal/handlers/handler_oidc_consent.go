package handlers

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"time"

	"github.com/google/uuid"

	"github.com/authelia/authelia/v4/internal/middlewares"
	"github.com/authelia/authelia/v4/internal/model"
	"github.com/authelia/authelia/v4/internal/oidc"
	"github.com/authelia/authelia/v4/internal/session"
	"github.com/authelia/authelia/v4/internal/utils"
)

// OpenIDConnectConsentGET handles requests to provide consent for OpenID Connect.
func OpenIDConnectConsentGET(ctx *middlewares.AutheliaCtx) {
	var (
		consentID uuid.UUID
		err       error
	)

	if consentID, err = uuid.Parse(string(ctx.RequestCtx.QueryArgs().Peek("consent_id"))); err != nil {
		ctx.Logger.Errorf("Unable to convert '%s' into a UUID: %+v", ctx.RequestCtx.QueryArgs().Peek("consent_id"), err)
		ctx.ReplyForbidden()

		return
	}

	userSession, consent, client, handled := oidcConsentGetSessionsAndClient(ctx, consentID)
	if handled {
		return
	}

	if !client.IsAuthenticationLevelSufficient(userSession.AuthenticationLevel) {
		ctx.Logger.Errorf("Unable to perform consent without sufficient authentication for user '%s' and client id '%s'", userSession.Username, consent.ClientID)
		ctx.ReplyForbidden()

		return
	}

	if err = ctx.SetJSONBody(client.GetConsentResponseBody(consent)); err != nil {
		ctx.Error(fmt.Errorf("unable to set JSON body: %v", err), "Operation failed")
	}
}

// OpenIDConnectConsentPOST handles consent responses for OpenID Connect.
func OpenIDConnectConsentPOST(ctx *middlewares.AutheliaCtx) {
	var (
		consentID uuid.UUID
		bodyJSON  oidc.ConsentPostRequestBody
		err       error
	)

	if err = json.Unmarshal(ctx.Request.Body(), &bodyJSON); err != nil {
		ctx.Logger.Errorf("Failed to parse JSON bodyJSON in consent POST: %+v", err)
		ctx.SetJSONError(messageOperationFailed)

		return
	}

	if consentID, err = uuid.Parse(bodyJSON.ConsentID); err != nil {
		ctx.Logger.Errorf("Unable to convert '%s' into a UUID: %+v", ctx.RequestCtx.QueryArgs().Peek("consent_id"), err)
		ctx.ReplyForbidden()

		return
	}

	userSession, consent, client, handled := oidcConsentGetSessionsAndClient(ctx, consentID)
	if handled {
		return
	}

	if !client.IsAuthenticationLevelSufficient(userSession.AuthenticationLevel) {
		ctx.Logger.Debugf("Insufficient permissions to give consent during POST current level: %d, require 2FA: %d", userSession.AuthenticationLevel, client.Policy)
		ctx.ReplyForbidden()

		return
	}

	if consent.ClientID != bodyJSON.ClientID {
		ctx.Logger.Errorf("User '%s' consented to scopes of another client (%s) than expected (%s). Beware this can be a sign of attack",
			userSession.Username, bodyJSON.ClientID, consent.ClientID)
		ctx.SetJSONError(messageOperationFailed)

		return
	}

	if bodyJSON.Consent {
		if bodyJSON.PreConfigure {
			if client.PreConfiguredConsentDuration == nil {
				ctx.Logger.Warnf("Consent session with challenge id '%s' for user '%s': consent pre-configuration was requested and was ignored because it is not permitted on this client", consent.ChallengeID.String(), userSession.Username)
			} else {
				expiresAt := time.Now().Add(*client.PreConfiguredConsentDuration)
				consent.ExpiresAt = &expiresAt

				ctx.Logger.Debugf("Consent session with challenge id '%s' for user '%s': pre-configured and set to expire at %v", consent.ChallengeID.String(), userSession.Username, consent.ExpiresAt)
			}
		}

		consent.GrantedScopes = consent.RequestedScopes
		consent.GrantedAudience = consent.RequestedAudience

		if !utils.IsStringInSlice(consent.ClientID, consent.GrantedAudience) {
			consent.GrantedAudience = append(consent.GrantedAudience, consent.ClientID)
		}
	}

	var externalRootURL string

	if externalRootURL, err = ctx.ExternalRootURL(); err != nil {
		ctx.Logger.Errorf("Could not determine the external URL during consent session processing with challenge id '%s' for user '%s': %v", consent.ChallengeID.String(), userSession.Username, err)
		ctx.SetJSONError(messageOperationFailed)

		return
	}

	if err = ctx.Providers.StorageProvider.SaveOAuth2ConsentSessionResponse(ctx, *consent, bodyJSON.Consent); err != nil {
		ctx.Logger.Errorf("Failed to save the consent session response to the database: %+v", err)
		ctx.SetJSONError(messageOperationFailed)

		return
	}

	var (
		redirectURI *url.URL
		query       url.Values
	)

	if redirectURI, err = url.ParseRequestURI(externalRootURL); err != nil {
		ctx.Logger.Errorf("Failed to parse the consent redirect URL: %+v", err)
		ctx.SetJSONError(messageOperationFailed)

		return
	}

	if query, err = url.ParseQuery(consent.Form); err != nil {
		ctx.Logger.Errorf("Failed to parse the consent form values: %+v", err)
		ctx.SetJSONError(messageOperationFailed)

		return
	}

	query.Set("consent_id", consent.ChallengeID.String())

	redirectURI.Path = path.Join(redirectURI.Path, oidc.AuthorizationPath)
	redirectURI.RawQuery = query.Encode()

	response := oidc.ConsentPostResponseBody{RedirectURI: redirectURI.String()}

	if err = ctx.SetJSONBody(response); err != nil {
		ctx.Error(fmt.Errorf("unable to set JSON bodyJSON in response"), "Operation failed")
	}
}

func oidcConsentGetSessionsAndClient(ctx *middlewares.AutheliaCtx, consentID uuid.UUID) (userSession session.UserSession, consent *model.OAuth2ConsentSession, client *oidc.Client, handled bool) {
	var (
		err error
	)

	userSession = ctx.GetSession()

	if consent, err = ctx.Providers.StorageProvider.LoadOAuth2ConsentSessionByChallengeID(ctx, consentID); err != nil {
		ctx.Logger.Errorf("Unable to load consent session with challenge id '%s': %v", consentID.String(), err)
		ctx.ReplyForbidden()

		return userSession, nil, nil, true
	}

	if client, err = ctx.Providers.OpenIDConnect.Store.GetFullClient(consent.ClientID); err != nil {
		ctx.Logger.Errorf("Unable to find related client configuration with name '%s': %v", consent.ClientID, err)
		ctx.ReplyForbidden()

		return userSession, nil, nil, true
	}

	var subject uuid.UUID

	if subject, err = ctx.Providers.OpenIDConnect.Store.GetSubject(ctx, client.GetSectorIdentifier(), userSession.Username); err != nil {
		ctx.Logger.Errorf("Unable to find user subject identifier for consent session '%s', user '%s', and sector '%s': %v", consent.ChallengeID, userSession.Username, client.GetSectorIdentifier(), err)
		ctx.ReplyForbidden()

		return userSession, nil, nil, true
	}

	if consent.Subject.UUID != subject {
		ctx.Logger.Errorf("Unable to match user subject identifier '%s' to the consent subject identifier '%s' for consent session '%s', user '%s', and sector '%s': %v", subject, consent.Subject.UUID, consent.ChallengeID, userSession.Username, client.GetSectorIdentifier(), err)
		ctx.ReplyForbidden()

		return userSession, nil, nil, true
	}

	return userSession, consent, client, false
}
