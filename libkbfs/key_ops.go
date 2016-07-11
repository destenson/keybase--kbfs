// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

// KeyOpsStandard implements the KeyOps interface and relays get/put
// requests for server-side key halves from/to the key server.
type KeyOpsStandard struct {
	config IFCERFTConfig
}

// Test that KeyOps standard fully implements the KeyOps interface.
var _ IFCERFTKeyOps = (*KeyOpsStandard)(nil)

// GetTLFCryptKeyServerHalf is an implementation of the KeyOps interface.
func (k *KeyOpsStandard) GetTLFCryptKeyServerHalf(ctx context.Context,
	serverHalfID IFCERFTTLFCryptKeyServerHalfID, key IFCERFTCryptPublicKey) (
	IFCERFTTLFCryptKeyServerHalf, error) {
	// get the key half from the server
	serverHalf, err := k.config.KeyServer().GetTLFCryptKeyServerHalf(ctx, serverHalfID, key)
	if err != nil {
		return IFCERFTTLFCryptKeyServerHalf{}, err
	}
	// get current uid and deviceKID
	_, uid, err := k.config.KBPKI().GetCurrentUserInfo(ctx)
	if err != nil {
		return IFCERFTTLFCryptKeyServerHalf{}, err
	}

	// verify we got the expected key
	crypto := k.config.Crypto()
	err = crypto.VerifyTLFCryptKeyServerHalfID(serverHalfID, uid, key.kid, serverHalf)
	if err != nil {
		return IFCERFTTLFCryptKeyServerHalf{}, err
	}
	return serverHalf, nil
}

// PutTLFCryptKeyServerHalves is an implementation of the KeyOps interface.
func (k *KeyOpsStandard) PutTLFCryptKeyServerHalves(ctx context.Context,
	serverKeyHalves map[keybase1.UID]map[keybase1.KID]IFCERFTTLFCryptKeyServerHalf) error {
	// upload the keys
	return k.config.KeyServer().PutTLFCryptKeyServerHalves(ctx, serverKeyHalves)
}

// DeleteTLFCryptKeyServerHalf is an implementation of the KeyOps interface.
func (k *KeyOpsStandard) DeleteTLFCryptKeyServerHalf(ctx context.Context,
	uid keybase1.UID, kid keybase1.KID,
	serverHalfID IFCERFTTLFCryptKeyServerHalfID) error {
	return k.config.KeyServer().DeleteTLFCryptKeyServerHalf(
		ctx, uid, kid, serverHalfID)
}
