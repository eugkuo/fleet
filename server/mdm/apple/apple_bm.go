package apple_mdm

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"

	abmctx "github.com/fleetdm/fleet/v4/server/contexts/apple_bm"
	"github.com/fleetdm/fleet/v4/server/contexts/ctxerr"
	"github.com/fleetdm/fleet/v4/server/fleet"
	"github.com/fleetdm/fleet/v4/server/mdm/assets"
	"github.com/fleetdm/fleet/v4/server/mdm/nanodep/client"
	depclient "github.com/fleetdm/fleet/v4/server/mdm/nanodep/client"
	"github.com/fleetdm/fleet/v4/server/mdm/nanodep/storage"
	kitlog "github.com/go-kit/log"
)

// SetABMTokenMetadata uses the provided ABM token to fetch the associated
// metadata and use it to update the rest of the abmToken fields (org name,
// apple ID, renew date). It only sets the data on the struct, it does not
// save it in the DB.
func SetABMTokenMetadata(
	ctx context.Context,
	abmToken *fleet.ABMToken,
	depStorage storage.AllDEPStorage,
	ds fleet.Datastore,
	logger kitlog.Logger,
) error {
	decryptedToken, err := assets.ABMToken(ctx, ds, abmToken.OrganizationName)
	if err != nil {
		return ctxerr.Wrap(ctx, err, "getting ABM token")
	}

	return SetDecryptedABMTokenMetadata(ctx, abmToken, decryptedToken, depStorage, ds, logger)
}

const UnsavedABMTokenOrgName = "new_abm_token" //nolint:gosec

func SetDecryptedABMTokenMetadata(
	ctx context.Context,
	abmToken *fleet.ABMToken,
	decryptedToken *depclient.OAuth1Tokens,
	depStorage storage.AllDEPStorage,
	ds fleet.Datastore,
	logger kitlog.Logger,
) error {
	depClient := NewDEPClient(depStorage, ds, logger)

	orgName := abmToken.OrganizationName
	if orgName == "" {
		// Then this is a newly uploaded token (or one migrated from the
		// single-token world), which will not be found in the datastore when
		// RetrieveAuthTokens tries to find it. Set the token in the context so
		// that downstream we know it's not in the datastore.
		ctx = abmctx.NewContext(ctx, decryptedToken)
		// We don't have an org name, but the depClient expects an org name, so we set this fake one.
		orgName = UnsavedABMTokenOrgName
	}

	res, err := depClient.AccountDetail(ctx, orgName)
	if err != nil {
		var authErr *depclient.AuthError
		if errors.As(err, &authErr) {
			// authentication failure with 401 unauthorized means that the configured
			// Apple BM certificate and/or token are invalid. Fail with a 400 Bad
			// Request.
			msg := err.Error()
			if authErr.StatusCode == http.StatusUnauthorized {
				msg = "The Apple Business Manager certificate or server token is invalid. Restart Fleet with a valid certificate and token. See https://fleetdm.com/learn-more-about/setup-abm for help."
			}
			return ctxerr.Wrap(ctx, &fleet.BadRequestError{
				Message:     msg,
				InternalErr: err,
			}, "apple GET /account request failed with authentication error")
		}
		return ctxerr.Wrap(ctx, err, "apple GET /account request failed")
	}

	if res.AdminID == "" {
		// fallback to facilitator ID, as this is the same information but for
		// older versions of the Apple API.
		// https://github.com/fleetdm/fleet/issues/7515#issuecomment-1346579398
		res.AdminID = res.FacilitatorID
	}

	abmToken.OrganizationName = res.OrgName
	abmToken.AppleID = res.AdminID
	abmToken.RenewAt = decryptedToken.AccessTokenExpiry.UTC()
	return nil
}

func DecryptedUploadedABMToken(ctx context.Context, ds fleet.Datastore, token io.Reader) (encryptedToken []byte, decryptedToken *client.OAuth1Tokens, err error) {
	pair, err := ds.GetAllMDMConfigAssetsByName(ctx, []fleet.MDMAssetName{
		fleet.MDMAssetABMCert,
		fleet.MDMAssetABMKey,
	})
	if err != nil {
		if fleet.IsNotFound(err) {
			return nil, nil, ctxerr.Wrap(ctx, &fleet.BadRequestError{
				Message: "Please generate a keypair first.",
			}, "renewing ABM token")
		}

		return nil, nil, ctxerr.Wrap(ctx, err, "retrieving stored ABM assets")
	}

	encryptedToken, err = io.ReadAll(token)
	if err != nil {
		return nil, nil, ctxerr.Wrap(ctx, err, "reading token bytes")
	}

	derCert, _ := pem.Decode(pair[fleet.MDMAssetABMCert].Value)
	if derCert == nil {
		return nil, nil, ctxerr.New(ctx, "ABM certificate in the database cannot be parsed")
	}

	cert, err := x509.ParseCertificate(derCert.Bytes)
	if err != nil {
		return nil, nil, ctxerr.Wrap(ctx, err, "parsing ABM certificate")
	}

	decryptedToken, err = assets.DecryptRawABMToken(encryptedToken, cert, pair[fleet.MDMAssetABMKey].Value)
	if err != nil {
		return nil, nil, ctxerr.Wrap(ctx, &fleet.BadRequestError{
			Message:     "Invalid token. Please provide a valid token from Apple Business Manager.",
			InternalErr: err,
		}, "validating ABM token")
	}
	return encryptedToken, decryptedToken, nil
}
