// Copyright 2021 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webauthn_test

import (
	"context"
	"encoding/pem"
	"testing"

	"github.com/duo-labs/webauthn/protocol"
	"github.com/google/go-cmp/cmp"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth/mocku2f"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"

	wanlib "github.com/gravitational/teleport/lib/auth/webauthn"
)

func TestRegistrationFlow_BeginFinish(t *testing.T) {
	const user = "llama"
	const rpID = "localhost"
	const origin = "https://localhost"
	identity := newFakeIdentity(user)
	webRegistration := wanlib.RegistrationFlow{
		Webauthn: &types.Webauthn{
			RPID: rpID,
		},
		Identity: identity,
	}

	ctx := context.Background()
	tests := []struct {
		name                 string
		user                 string
		deviceName           string
		passwordless         bool
		wantUserVerification protocol.UserVerificationRequirement
		wantResidentKey      bool
	}{
		{
			name:                 "MFA",
			user:                 user,
			deviceName:           "webauthn1",
			wantUserVerification: protocol.VerificationDiscouraged,
		},
		{
			name:                 "passwordless",
			user:                 user,
			deviceName:           "webauthn2",
			passwordless:         true,
			wantUserVerification: protocol.VerificationRequired,
			wantResidentKey:      true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity.UpdatedDevices = nil // Clear "stored" devices

			// Begin is the first step in registration.
			credentialCreation, err := webRegistration.Begin(ctx, user, test.passwordless)
			require.NoError(t, err)
			// Assert some parts of the credentialCreation (it's framework-created, no
			// need to go too deep).
			require.NotNil(t, credentialCreation)
			require.NotEmpty(t, credentialCreation.Response.Challenge)
			require.Equal(t, webRegistration.Webauthn.RPID, credentialCreation.Response.RelyingParty.ID)
			// Are we using the correct authenticator selection settings?
			require.Equal(t, test.wantResidentKey, *credentialCreation.Response.AuthenticatorSelection.RequireResidentKey)
			require.Equal(t, test.wantUserVerification, credentialCreation.Response.AuthenticatorSelection.UserVerification)
			// Did we record the SessionData in storage?
			require.NotEmpty(t, identity.SessionData)

			// Sign CredentialCreation, typically requires user interaction.
			dev, err := mocku2f.Create()
			require.NoError(t, err)
			dev.SetUV = test.wantUserVerification == protocol.VerificationRequired
			dev.AllowResidentKey = test.wantResidentKey
			ccr, err := dev.SignCredentialCreation(origin, credentialCreation)
			require.NoError(t, err)

			// Finish is the final step in registration.
			newDevice, err := webRegistration.Finish(ctx, user, test.deviceName, ccr)
			require.NoError(t, err)
			require.Equal(t, test.deviceName, newDevice.GetName())
			// Did we get a proper WebauthnDevice?
			gotDevice := newDevice.GetWebauthn()
			require.NotNil(t, gotDevice)
			require.NotEmpty(t, gotDevice.PublicKeyCbor) // validated indirectly via authentication
			wantDevice := &types.WebauthnDevice{
				CredentialId:      dev.KeyHandle,
				PublicKeyCbor:     gotDevice.PublicKeyCbor,
				AttestationType:   gotDevice.AttestationType,
				Aaguid:            make([]byte, 16), // 16 zeroes
				SignatureCounter:  0,
				AttestationObject: ccr.AttestationResponse.AttestationObject,
				ResidentKey:       test.wantResidentKey,
			}
			if diff := cmp.Diff(wantDevice, gotDevice); diff != "" {
				t.Errorf("Finish() mismatch (-want +got):\n%s", diff)
			}
			// SessionData was cleared?
			require.Empty(t, identity.SessionData)
			// Device created in storage?
			require.Len(t, identity.UpdatedDevices, 1)
			require.Equal(t, newDevice, identity.UpdatedDevices[0])
		})
	}
}

func TestRegistrationFlow_Begin_webID(t *testing.T) {
	const rpID = "localhost"
	ctx := context.Background()

	alpacaWebID := []byte{1, 2, 3, 4, 5}
	tests := []struct {
		name             string
		user, mappedUser string
		wla              *types.WebauthnLocalAuth
	}{
		{
			name: "user without webID", // first registration or U2F user
			user: "llama",
		},
		{
			name:       "user without webID mapping", // aka legacy or inconsistent storage
			user:       "alpaca",
			mappedUser: "", // missing mapping
			wla:        &types.WebauthnLocalAuth{UserID: alpacaWebID},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user := test.user

			// Prepare identity/user according to test parameters.
			identity := newFakeIdentity(user)
			if test.wla != nil {
				err := identity.UpsertWebauthnLocalAuth(ctx, user, test.wla)
				require.NoError(t, err, "failed to upsert WebauthnLocalAuth")
			}
			identity.MappedUser = test.mappedUser

			// Begin registration; this should create/fix the webID and webID->user
			// mappings in storage.
			webRegistration := &wanlib.RegistrationFlow{
				Webauthn: &types.Webauthn{RPID: rpID},
				Identity: identity,
			}
			_, err := webRegistration.Begin(ctx, user, false /* passwordless */)
			require.NoError(t, err, "Begin failed")

			// Verify that we have both the webID and the correct webID->user
			// mapping.
			wla, err := identity.GetWebauthnLocalAuth(ctx, user)
			require.NoError(t, err, "failed to read WebauthnLocalAuth")
			require.NotEmpty(t, wla.UserID)

			gotUser, err := identity.GetTeleportUserByWebauthnID(ctx, wla.UserID)
			require.NoError(t, err, "failed to get user from webID")
			require.Equal(t, user, gotUser)
		})
	}
}

func TestRegistrationFlow_Begin_errors(t *testing.T) {
	const user = "llama"
	webRegistration := &wanlib.RegistrationFlow{
		Webauthn: &types.Webauthn{
			RPID: "localhost",
		},
		Identity: newFakeIdentity(user),
	}

	ctx := context.Background()
	_, err := webRegistration.Begin(ctx, "" /* user */, false /* passwordless */)
	require.True(t, trace.IsBadParameter(err)) // user required
}

func TestRegistrationFlow_Finish_errors(t *testing.T) {
	const user = "llama"
	const webOrigin = "https://localhost"
	webRegistration := &wanlib.RegistrationFlow{
		Webauthn: &types.Webauthn{
			RPID: "localhost",
		},
		Identity: newFakeIdentity(user),
	}

	ctx := context.Background()

	cc, err := webRegistration.Begin(ctx, user, false /* passwordless */)
	require.NoError(t, err)
	key, err := mocku2f.Create()
	require.NoError(t, err)
	okCCR, err := key.SignCredentialCreation(webOrigin, cc)
	require.NoError(t, err)

	tests := []struct {
		name             string
		user, deviceName string
		createResp       func() *wanlib.CredentialCreationResponse
		wantErr          string
	}{
		{
			name:       "NOK user empty",
			user:       "",
			deviceName: "webauthn2",
			createResp: func() *wanlib.CredentialCreationResponse { return okCCR },
			wantErr:    "user required",
		},
		{
			name:       "NOK device name empty",
			user:       user,
			deviceName: "",
			createResp: func() *wanlib.CredentialCreationResponse { return okCCR },
			wantErr:    "device name required",
		},
		{
			name:       "NOK credential response nil",
			user:       user,
			deviceName: "webauthn2",
			createResp: func() *wanlib.CredentialCreationResponse { return nil },
			wantErr:    "response required",
		},
		{
			name:       "NOK credential with bad origin",
			user:       user,
			deviceName: "webauthn2",
			createResp: func() *wanlib.CredentialCreationResponse {
				resp, err := key.SignCredentialCreation("https://alpacasarerad.com" /* origin */, cc)
				require.NoError(t, err)
				return resp
			},
			wantErr: "origin",
		},
		{
			name:       "NOK credential with bad RPID",
			user:       user,
			deviceName: "webauthn2",
			createResp: func() *wanlib.CredentialCreationResponse {
				cc, err := webRegistration.Begin(ctx, user, false /* passwordless */)
				require.NoError(t, err)
				cc.Response.RelyingParty.ID = "badrpid.com"

				resp, err := key.SignCredentialCreation(webOrigin, cc)
				require.NoError(t, err)
				return resp
			},
			wantErr: "authenticator response",
		},
		{
			name:       "NOK credential with invalid signature",
			user:       user,
			deviceName: "webauthn2",
			createResp: func() *wanlib.CredentialCreationResponse {
				cc, err := webRegistration.Begin(ctx, user, false /* passwordless */)
				require.NoError(t, err)
				// Flip a challenge bit, this should be enough to consistently fail
				// signature checking.
				cc.Response.Challenge[0] = 1 ^ cc.Response.Challenge[0]

				resp, err := key.SignCredentialCreation(webOrigin, cc)
				require.NoError(t, err)
				return resp
			},
			wantErr: "validating challenge",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := webRegistration.Finish(ctx, test.user, test.deviceName, test.createResp())
			require.Error(t, err)
			require.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestRegistrationFlow_Finish_attestation(t *testing.T) {
	const rpID = "localhost"
	const origin = "https://localhost"
	const user = "llama"
	const devName = "web1" // OK to repeat, discarded between tests.

	dev1, err := mocku2f.Create()
	require.NoError(t, err)
	dev2, err := mocku2f.Create()
	require.NoError(t, err)
	dev3, err := mocku2f.Create()
	require.NoError(t, err)

	tests := []struct {
		name                  string
		allowedCAs, deniedCAs [][]byte
		dev                   *mocku2f.Key
		wantOK                bool
	}{
		{
			name:       "OK Device clears allow list",
			allowedCAs: [][]byte{dev1.Cert, dev2.Cert},
			dev:        dev1,
			wantOK:     true,
		},
		{
			name:      "OK Device clears deny list",
			deniedCAs: [][]byte{dev1.Cert, dev2.Cert},
			dev:       dev3,
			wantOK:    true,
		},
		{
			name:       "OK Device clears allow and deny lists",
			allowedCAs: [][]byte{dev1.Cert},
			deniedCAs:  [][]byte{dev2.Cert, dev3.Cert},
			dev:        dev1,
			wantOK:     true,
		},
		{
			name:       "NOK Device not allowed",
			allowedCAs: [][]byte{dev1.Cert, dev2.Cert},
			dev:        dev3,
		},
		{
			name:      "NOK Device denied",
			deniedCAs: [][]byte{dev1.Cert, dev2.Cert},
			dev:       dev1,
		},
		{
			name:       "NOK Device denied (allow plus deny version)",
			allowedCAs: [][]byte{dev1.Cert},
			// Usually, in this case, the allowed CA would be broader than the denied
			// CA, but we are going for a simplified (albeit odd) scenario in the
			// test.
			deniedCAs: [][]byte{dev1.Cert},
			dev:       dev1,
		},
	}

	ctx := context.Background()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			webRegistration := wanlib.RegistrationFlow{
				Webauthn: &types.Webauthn{
					RPID:                  rpID,
					AttestationAllowedCAs: derToPEMs(test.allowedCAs),
					AttestationDeniedCAs:  derToPEMs(test.deniedCAs),
				},
				Identity: newFakeIdentity(user),
			}

			cc, err := webRegistration.Begin(ctx, user, false /* passwordless */)
			require.NoError(t, err)

			dev := test.dev
			ccr, err := dev.SignCredentialCreation(origin, cc)
			require.NoError(t, err)

			_, err = webRegistration.Finish(ctx, user, devName, ccr)
			if ok := err == nil; ok != test.wantOK {
				t.Errorf("Finish returned err = %v, wantOK = %v", err, test.wantOK)
			}
		})
	}
}

func derToPEMs(certs [][]byte) []string {
	res := make([]string, len(certs))
	for i, cert := range certs {
		certPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert,
		})
		res[i] = string(certPEM)
	}
	return res
}
